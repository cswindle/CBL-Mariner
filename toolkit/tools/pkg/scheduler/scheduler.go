package scheduler

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"

	"github.com/juliangruber/go-intersect"
	"golang.org/x/sys/unix"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/exe"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkgjson"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/shell"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/graph/pkggraph"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/scheduler/buildagents"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/scheduler/schedulerutils"
)

// schedulerChannels represents the communication channels used by a build agent.
// Unlike BuildChannels, schedulerChannels holds bidirectional channels that
// only the top-level scheduler should have. BuildChannels contains directional channels.
type schedulerChannels struct {
	Requests         chan *schedulerutils.BuildRequest
	PriorityRequests chan *schedulerutils.BuildRequest
	Results          chan *schedulerutils.BuildResult
	Cancel           chan struct{}
	Done             chan struct{}
}

func ScheduleBuild(cfg *Config) error {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
		logger.Log.Debugf("No worker count supplied, discovered %d logical CPUs.", cfg.Workers)
	}

	if cfg.BuildAttempts <= 0 {
		logger.Log.Fatalf("Value in --build-attempts must be greater than zero. Found %d", cfg.BuildAttempts)
	}

	ignoredPackages := exe.ParseListArgument(cfg.IgnoredPackages)
	reservedFileListFile := cfg.ReservedFileListFile

	// Generate the list of packages that need to be built.
	// If none are requested then all packages will be built.
	packagesNamesToBuild := exe.ParseListArgument(cfg.PkgsToBuild)
	packagesNamesToRebuild := exe.ParseListArgument(cfg.PkgsToRebuild)

	ignoredAndRebuiltPackages := intersect.Hash(ignoredPackages, packagesNamesToRebuild)
	if len(ignoredAndRebuiltPackages) != 0 {
		logger.Log.Fatalf("Can't ignore and force a rebuild of a package at the same time. Abusing packages: %v", ignoredAndRebuiltPackages)
	}

	packageVersToBuild, err := schedulerutils.CalculatePackagesToBuild(packagesNamesToBuild, packagesNamesToRebuild, cfg.InputGraphFile, cfg.ImageConfig, cfg.BaseDirPath)
	if err != nil {
		logger.Log.Fatalf("Unable to generate package build list, error: %s", err)
	}

	var reservedFiles []string
	if len(reservedFileListFile) > 0 {
		reservedFiles, err = schedulerutils.ReadReservedFilesList(reservedFileListFile)
		if err != nil {
			logger.Log.Fatalf("unable to read reserved file list %s: %s", reservedFileListFile, err)
		}
	}

	// Setup a build agent to handle build requests from the scheduler.
	buildAgentConfig := &buildagents.BuildAgentConfig{
		Program:              cfg.BuildAgentProgram,
		CacheDir:             cfg.CacheDir,
		RepoFile:             cfg.RepoFile,
		RpmDir:               cfg.RpmDir,
		SrpmDir:              cfg.SrpmDir,
		WorkDir:              cfg.WorkDir,
		WorkerTar:            cfg.WorkerTar,
		DistTag:              cfg.DistTag,
		DistroReleaseVersion: cfg.DistroReleaseVersion,
		DistroBuildNumber:    cfg.DistroBuildNumber,
		RpmmacrosFile:        cfg.RpmmacrosFile,
		NoCleanup:            cfg.NoCleanup,
		RunCheck:             cfg.RunCheck,
		LogDir:               cfg.BuildLogsDir,
		LogLevel:             cfg.LogLevel,
	}

	agent, err := buildagents.BuildAgentFactory(cfg.BuildAgent)
	if err != nil {
		logger.Log.Fatalf("Unable to select build agent, error: %s", err)
	}

	err = agent.Initialize(buildAgentConfig)
	if err != nil {
		logger.Log.Fatalf("Unable to initialize build agent, error: %s", err)
	}

	// Setup cleanup routines to ensure no builds are left running when scheduler is exiting.
	// Ensure no outstanding agents are running on graceful exit
	defer cancelOutstandingBuilds(agent)
	// On a SIGINT or SIGTERM stop all agents.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, unix.SIGINT, unix.SIGTERM)
	go cancelBuildsOnSignal(signals, agent)

	err = buildGraph(cfg.InputGraphFile, cfg.OutputGraphFile, agent, cfg.Workers, cfg.BuildAttempts, cfg.StopOnFailure, !cfg.NoCache, packageVersToBuild, packagesNamesToRebuild, ignoredPackages, reservedFiles)
	return err
}

// cancelOutstandingBuilds stops any builds that are currently running.
func cancelOutstandingBuilds(agent buildagents.BuildAgent) {
	err := agent.Close()
	if err != nil {
		logger.Log.Errorf("Unable to close build agent, error: %s", err)
	}

	// Issue a SIGINT to all children processes to allow them to gracefully exit.
	shell.PermanentlyStopAllProcesses(unix.SIGINT)
}

// cancelBuildsOnSignal will stop any builds running on SIGINT/SIGTERM.
func cancelBuildsOnSignal(signals chan os.Signal, agent buildagents.BuildAgent) {
	sig := <-signals
	logger.Log.Error(sig)

	cancelOutstandingBuilds(agent)
	os.Exit(1)
}

// buildGraph builds all packages in the dependency graph requested.
// It will save the resulting graph to outputFile.
func buildGraph(inputFile, outputFile string, agent buildagents.BuildAgent, workers, buildAttempts int, stopOnFailure, canUseCache bool, packagesToBuild []*pkgjson.PackageVer, packagesNamesToRebuild, ignoredPackages, reservedFiles []string) (err error) {
	// graphMutex guards pkgGraph from concurrent reads and writes during build.
	var graphMutex sync.RWMutex

	isGraphOptimized, pkgGraph, goalNode, err := schedulerutils.InitializeGraph(inputFile, packagesToBuild)
	if err != nil {
		return
	}

	// Setup and start the worker pool and scheduler routine.
	numberOfNodes := pkgGraph.Nodes().Len()

	channels := startWorkerPool(agent, workers, buildAttempts, numberOfNodes, &graphMutex, ignoredPackages)
	logger.Log.Infof("Building %d nodes with %d workers", numberOfNodes, workers)

	// After this call pkgGraph will be given to multiple routines and accessing it requires acquiring the mutex.
	builtGraph, err := buildAllNodes(stopOnFailure, isGraphOptimized, canUseCache, packagesNamesToRebuild, pkgGraph, &graphMutex, goalNode, channels, reservedFiles)

	if builtGraph != nil {
		graphMutex.RLock()
		defer graphMutex.RUnlock()

		saveErr := pkggraph.WriteDOTGraphFile(builtGraph, outputFile)
		if saveErr != nil {
			logger.Log.Errorf("Failed to save built graph, error: %s", saveErr)
		}
	}

	return
}

// startWorkerPool starts the worker pool and returns the communication channels between the workers and the scheduler.
// channelBufferSize controls how many entries in the channels can be buffered before blocking writes to them.
func startWorkerPool(agent buildagents.BuildAgent, workers, buildAttempts, channelBufferSize int, graphMutex *sync.RWMutex, ignoredPackages []string) (channels *schedulerChannels) {
	channels = &schedulerChannels{
		Requests:         make(chan *schedulerutils.BuildRequest, channelBufferSize),
		PriorityRequests: make(chan *schedulerutils.BuildRequest, channelBufferSize),
		Results:          make(chan *schedulerutils.BuildResult, channelBufferSize),
		Cancel:           make(chan struct{}),
		Done:             make(chan struct{}),
	}

	// Downcast the bidirectional scheduler channels into directional channels for the build workers.
	directionalChannels := &schedulerutils.BuildChannels{
		Requests:         channels.Requests,
		PriorityRequests: channels.PriorityRequests,
		Results:          channels.Results,
		Cancel:           channels.Cancel,
		Done:             channels.Done,
	}

	// Start the workers now so they begin working as soon as a new job is queued.
	for i := 0; i < workers; i++ {
		logger.Log.Debugf("Starting worker #%d", i)
		go schedulerutils.BuildNodeWorker(directionalChannels, agent, graphMutex, buildAttempts, ignoredPackages)
	}

	return
}

// buildAllNodes will build all nodes in a given dependency graph.
// This routine only contains control flow logic for build scheduling.
// It iteratively:
// - Calculates any unblocked nodes.
// - Submits these nodes to the worker pool to be processed.
// - Grabs a single build result from the worker pool.
// - Attempts to satisfy any unresolved dynamic dependencies with new implicit provides from the build result.
// - Attempts to subgraph the graph to only contain the requested packages if possible.
// - Repeat.
func buildAllNodes(stopOnFailure, isGraphOptimized, canUseCache bool, packagesNamesToRebuild []string, pkgGraph *pkggraph.PkgGraph, graphMutex *sync.RWMutex, goalNode *pkggraph.PkgNode, channels *schedulerChannels, reservedFiles []string) (builtGraph *pkggraph.PkgGraph, err error) {
	var (
		// stopBuilding tracks if the build has entered a failed state and this routine should stop as soon as possible.
		stopBuilding bool
		// useCachedImplicit tracks if cached implicit provides can be used to satisfy unresolved dynamic dependencies.
		// Local packages are preferred over cached remotes ones to satisfy these unresolved dependencies, however
		// the scheduler does not know what packages provide which implicit provides until the packages have been built.
		// Therefore the scheduler will attempt to build all possible packages without consuming any cached dynamic dependencies first.
		useCachedImplicit bool
	)

	// Start the build at the leaf nodes.
	// The build will bubble up through the graph as it processes nodes.
	buildState := schedulerutils.NewGraphBuildState(reservedFiles)
	nodesToBuild := schedulerutils.LeafNodes(pkgGraph, graphMutex, goalNode, buildState, useCachedImplicit)

	for {
		logger.Log.Debugf("Found %d unblocked nodes", len(nodesToBuild))

		// Each node that is ready to build must be converted into a build request and submitted to the worker pool.
		newRequests := schedulerutils.ConvertNodesToRequests(pkgGraph, graphMutex, nodesToBuild, packagesNamesToRebuild, buildState, canUseCache)
		for _, req := range newRequests {
			buildState.RecordBuildRequest(req)
			// Decide which priority the build should be. Generally we want to get any remote or prebuilt nodes out of the
			// way as quickly as possible since they may help us optimize the graph early.
			// Meta nodes may also be blocking something we want to examine and give higher priority (priority inheritance from
			// the hypothetical high priority node hidden further into the tree)
			switch req.Node.Type {
			case pkggraph.TypePreBuilt:
				channels.PriorityRequests <- req

				// For now all build nodes are of equal priority
			case pkggraph.TypeGoal:
				fallthrough
			case pkggraph.TypePureMeta:
				fallthrough
			case pkggraph.TypeRun:
				fallthrough
			case pkggraph.TypeRemote:
				fallthrough
			case pkggraph.TypeBuild:
				fallthrough
			default:
				channels.Requests <- req
			}
		}
		nodesToBuild = nil

		// If there are no active builds running try enabling cached packages for unresolved dynamic dependencies to unblocked more nodes.
		// Otherwise there is nothing left that can be built.
		if len(buildState.ActiveBuilds()) == 0 {
			if useCachedImplicit {
				err = fmt.Errorf("could not build all packages")
				break
			} else {
				logger.Log.Warn("Enabling cached packages to satisfy unresolved dynamic dependencies.")
				useCachedImplicit = true
				nodesToBuild = schedulerutils.LeafNodes(pkgGraph, graphMutex, goalNode, buildState, useCachedImplicit)
				continue
			}
		}

		// Process the the next build result
		res := <-channels.Results
		schedulerutils.PrintBuildResult(res)
		buildState.RecordBuildResult(res)

		if !stopBuilding {
			if res.Err == nil {
				// If the graph has already been optimized and is now solvable without any additional information
				// then skip processing any new implicit provides.
				if !isGraphOptimized {
					var (
						didOptimize bool
						newGraph    *pkggraph.PkgGraph
						newGoalNode *pkggraph.PkgNode
					)
					didOptimize, newGraph, newGoalNode, err = updateGraphWithImplicitProvides(res, pkgGraph, graphMutex, useCachedImplicit)
					if err != nil {
						// Failures to manipulate the graph are fatal.
						// There is no guarantee the graph is still a directed acyclic graph and is solvable.
						stopBuilding = true
						stopBuild(channels, buildState)
					} else if didOptimize {
						isGraphOptimized = true
						// Replace the graph and goal node pointers.
						// Any outstanding builds of nodes that are no longer in the graph will gracefully handle this.
						// When querying their edges, the graph library will return an empty iterator (graph.Empty).
						pkgGraph = newGraph
						goalNode = newGoalNode
					}
				}

				nodesToBuild = schedulerutils.FindUnblockedNodesFromResult(res, pkgGraph, graphMutex, buildState)
			} else if stopOnFailure {
				stopBuilding = true
				err = res.Err
				stopBuild(channels, buildState)
			}
		}

		// If the goal node is available, mark the build as stopping.
		// There may still be outstanding builds if the graph was recently subgraphed
		// due to an unresolved implicit provide being satisfied and nodes that are no
		// longer in the graph are building.
		if buildState.IsNodeAvailable(goalNode) {
			logger.Log.Infof("All packages built")
			stopBuilding = true
		}

		activeSRPMs := buildState.ActiveSRPMs()
		activeSRPMsCount := len(activeSRPMs)
		if stopBuilding {
			if activeSRPMsCount == 0 {
				break
			}
		}

		if res.Node.Type == pkggraph.TypeBuild {
			logger.Log.Infof("%d currently active build(s): %v.", activeSRPMsCount, activeSRPMs)
		}
	}

	// Let the workers know they are done
	doneBuild(channels, buildState)
	// Give the workers time to finish so they don't mess up the summary we want to print.
	// Some nodes may still be busy with long running builds we don't care about anymore, so we don't
	// want to actually block here.
	time.Sleep(time.Second)

	builtGraph = pkgGraph
	schedulerutils.PrintBuildSummary(builtGraph, graphMutex, buildState)

	return
}

// updateGraphWithImplicitProvides will update the graph with new implicit provides if available.
// It will also attempt to subgraph the graph if it becomes solvable with the new implicit provides.
func updateGraphWithImplicitProvides(res *schedulerutils.BuildResult, pkgGraph *pkggraph.PkgGraph, graphMutex *sync.RWMutex, useCachedImplicit bool) (didOptimize bool, newGraph *pkggraph.PkgGraph, newGoalNode *pkggraph.PkgNode, err error) {
	// acquire a writer lock since this routine will collapse nodes
	graphMutex.Lock()
	defer graphMutex.Unlock()

	didInjectAny, err := schedulerutils.InjectMissingImplicitProvides(res, pkgGraph, useCachedImplicit)
	if err != nil {
		logger.Log.Errorf("Failed to add implicit provides for (%s). Error: %s", res.Node.FriendlyName(), err)
	} else if didInjectAny {
		// Failure to optimize the graph is non fatal as there may simply be unresolved dynamic dependencies
		var subgraphErr error
		newGraph, newGoalNode, subgraphErr = schedulerutils.OptimizeGraph(pkgGraph, useCachedImplicit)
		if subgraphErr == nil {
			logger.Log.Infof("Created solvable subgraph with new implicit provide information")
			didOptimize = true
		}
	}

	return
}

func drainChannels(channels *schedulerChannels, buildState *schedulerutils.GraphBuildState) {
	// For any workers that are current parked with no buffered requests, close the
	// requests channel to wake up any build workers waiting on a request to be buffered.
	// Upon being woken up by a closed requests channel, the build worker will stop.
	close(channels.Requests)
	close(channels.PriorityRequests)

	// Drain the request buffers to sync the build state with the new number of outstanding builds.
	for req := range channels.PriorityRequests {
		buildState.RemoveBuildRequest(req)
	}
	for req := range channels.Requests {
		buildState.RemoveBuildRequest(req)
	}
}

func doneBuild(channels *schedulerChannels, buildState *schedulerutils.GraphBuildState) {
	// Close the done channel. The build workers will finish processing any work, then return
	// upon seeing this channel is closed.
	close(channels.Done)

	drainChannels(channels, buildState)
}

// stopBuild will stop all future builds from being scheduled by sending a cancellation signal
// to the worker pool and draining any outstanding build requests.
func stopBuild(channels *schedulerChannels, buildState *schedulerutils.GraphBuildState) {
	logger.Log.Error("Stopping build")

	// Close the cancel channel to prevent and buffered requests from being built.
	// Upon seeing the cancel channel is closed, the build worker will stop instead
	// of processing a new request.
	close(channels.Cancel)

	drainChannels(channels, buildState)
}
