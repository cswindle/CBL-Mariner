// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/exe"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/validatechroot"

	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	leaveChrootFilesOnDisk = false
)

var (
	app = kingpin.New("validatechroot", "A tool to validate that the worker chroot is well configured and all dependencies are satisfied.")

	toolchainRpmsDir = app.Flag("rpm-dir", "Directory that contains already built toolchain RPMs. Should contain top level directories for architecture.").Required().ExistingDir()
	tmpDir           = app.Flag("tmp-dir", "Temporary chroot directory.").String()

	workerTar      = app.Flag("worker-chroot", "Full path to worker_chroot.tar.gz").Required().ExistingFile()
	workerManifest = app.Flag("worker-manifest", "Full path to the worker manifest file").Required().ExistingFile()

	logFile  = exe.LogFileFlag(app)
	logLevel = exe.LogLevelFlag(app)
)

func main() {
	app.Version(exe.ToolkitVersion)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger.InitBestEffort(*logFile, *logLevel)

	err := validatechroot.Validate(nil)
	if err != nil {
		logger.Log.Fatalf("Failed to validate worker. Error: %s", err)
	}
}
