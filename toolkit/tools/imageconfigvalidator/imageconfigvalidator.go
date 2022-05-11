// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// An image configuration validator

package main

import (
	"os"
	"path/filepath"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/exe"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/image/configvalidator"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/imagegen/configuration"
)

var (
	app = kingpin.New("imageconfigvalidator", "A tool for validating image configuration files")

	logFile  = exe.LogFileFlag(app)
	logLevel = exe.LogLevelFlag(app)

	input       = exe.InputStringFlag(app, "Path to the image config file.")
	baseDirPath = exe.InputDirFlag(app, "Base directory for relative file paths from the config.")
)

func main() {
	const returnCodeOnError = 1

	app.Version(exe.ToolkitVersion)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger.InitBestEffort(*logFile, *logLevel)

	inPath, err := filepath.Abs(*input)
	logger.PanicOnError(err, "Error when calculating input path")
	baseDir, err := filepath.Abs(*baseDirPath)
	logger.PanicOnError(err, "Error when calculating input directory")

	logger.Log.Infof("Reading configuration file (%s)", inPath)
	config, err := configuration.LoadWithAbsolutePaths(inPath, baseDir)
	if err != nil {
		logger.Log.Fatalf("Failed while loading image configuration '%s': %s", inPath, err)
	}

	// Basic validation will occur during load, but we can add additional checking here.
	err = configvalidator.ValidateConfiguration(config)
	if err != nil {
		// Log an error here as opposed to panicing to keep the output simple
		// and only contain the error with the config file.
		logger.Log.Fatalf("Invalid configuration '%s': %s", inPath, err)
	}

	return
}
