package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"github.com/bitrise-io/go-steputils/v2/stepconf"
	"github.com/bitrise-io/go-utils/v2/command"
	"github.com/bitrise-io/go-utils/v2/env"
	"github.com/bitrise-io/go-utils/v2/fileutil"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-io/go-utils/v2/pathutil"
	"github.com/bitrise-steplib/steps-xcode-archive/step"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := log.NewLogger()
	archiver := createXcodebuildArchiver(logger)
	config, err := archiver.ProcessInputs()
	if err != nil {
		logger.Errorf(formattedError(fmt.Errorf("Failed to process Step inputs: %w", err)))
		return 1
	}

	dependenciesOpts := step.EnsureDependenciesOpts{
		XCPretty: config.LogFormatter == "xcpretty",
	}
	if err := archiver.EnsureDependencies(dependenciesOpts); err != nil {
		var xcprettyInstallErr step.XCPrettyInstallError
		if errors.As(err, &xcprettyInstallErr) {
			logger.Warnf("Installing xcpretty failed: %s", err)
			logger.Warnf("Switching to xcodebuild for log formatter")
			config.LogFormatter = "xcodebuild"
		} else {
			logger.Errorf(formattedError(fmt.Errorf("Failed to install Step dependencies: %w", err)))
			return 1
		}
	}

	maxRetries := config.MaxRetryCount
	if maxRetries < 1 {
		maxRetries = 1
	}
	
	var result step.RunResult
	var runErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			logger.Infof("Archive attempt %d of %d", attempt, maxRetries)
			// Perform explicit clean and disable cache
			cleanArgs := []string{"clean"}
			if strings.HasSuffix(config.ProjectPath, ".xcworkspace") {
				cleanArgs = append(cleanArgs, "-workspace", config.ProjectPath)
			} else {
				cleanArgs = append(cleanArgs, "-project", config.ProjectPath)
			}
			cleanArgs = append(cleanArgs, "-scheme", config.Scheme)
			
			cleanCmd := exec.Command("xcodebuild", cleanArgs...)
			logger.Infof("Performing clean: %s", cleanCmd.String())
			if output, err := cleanCmd.CombinedOutput(); err != nil {
				logger.Warnf("Failed to clean project: %s", err)
				logger.Warnf("Clean command output: %s", string(output))
			}
			// Clear Xcode caches and derived data
			cmd := exec.Command("rm", "-rf", filepath.Join(os.Getenv("HOME"), "Library/Caches/com.apple.dt.Xcode"))
			logger.Infof("Cleaning xcode cache: %s", cmd.String())
			if output, err := cmd.CombinedOutput(); err != nil {
				logger.Warnf("Failed to clear Xcode caches: %s", err)
				logger.Warnf("Xcode cache command output: %s", string(output))
			}			
			cmd = exec.Command("rm", "-rf", filepath.Join(os.Getenv("HOME"), "Library/Caches/org.swift.swiftpm"))
			logger.Infof("Cleaning Swift Package Manager cache: %s", cmd.String())
			if output, err := cmd.CombinedOutput(); err != nil {
				logger.Warnf("Failed to clear Swift Package Manager cache: %s", err)
				logger.Warnf("Swift Package Manager cache command output: %s", string(output))
			}			
			cmd = exec.Command("rm", "-rf", filepath.Join(os.Getenv("HOME"), "Library/Developer/Xcode/DerivedData/*"))
			logger.Infof("Cleaning derived data: %s", cmd.String())
			if output, err := cmd.CombinedOutput(); err != nil {
				logger.Warnf("Failed to clear derived data: %s", err)
				logger.Warnf("Derived data command output: %s", string(output))
			}
			// Clear build state cache
			cmd = exec.Command("rm", "-rf", filepath.Join(os.Getenv("HOME"), "Library/Developer/Xcode/BuildState/*"))
			logger.Infof("Cleaning build state cache: %s", cmd.String())
			if output, err := cmd.CombinedOutput(); err != nil {
				logger.Warnf("Failed to clear build state cache: %s", err)
				logger.Warnf("Build state cache command output: %s", string(output))
			}

			// Generate project using tuist
			tuistCmd := exec.Command("tuist", "generate", "--configuration", config.Configuration, "-p", "tuist")
			logger.Infof("Generating project with tuist: %s", tuistCmd.String())
			if output, err := tuistCmd.CombinedOutput(); err != nil {
				logger.Warnf("Failed to generate project with tuist: %s", err)
				logger.Warnf("Tuist command output: %s", string(output))
			}

			config.CacheLevel = "none"
			time.Sleep(30 * time.Second)
		}

		runOpts := createRunOptions(config)
		result, runErr = archiver.Run(runOpts)
		if runErr == nil {
			break
		}

		if attempt < maxRetries {
			logger.Warnf("Archive failed, will retry: %s", runErr)
		}
	}

	exitCode := 0
	if runErr != nil {
		logger.Errorf(formattedError(fmt.Errorf("Failed to execute Step main logic after %d attempts: %w", maxRetries, runErr)))
		exitCode = 1
		// don't return as step outputs needs to be exported even in case of failure (for example the xcodebuild logs)
	}

	exportOpts := createExportOptions(config, result)
	if err := archiver.ExportOutput(exportOpts); err != nil {
		logger.Errorf(formattedError(fmt.Errorf("Failed to export Step outputs: %w", err)))
		return 1
	}

	return exitCode
}

func createXcodebuildArchiver(logger log.Logger) step.XcodebuildArchiver {
	xcodeVersionProvider := step.NewXcodebuildXcodeVersionProvider()
	envRepository := env.NewRepository()
	inputParser := stepconf.NewInputParser(envRepository)
	pathProvider := pathutil.NewPathProvider()
	pathChecker := pathutil.NewPathChecker()
	pathModifier := pathutil.NewPathModifier()
	fileManager := fileutil.NewFileManager()
	cmdFactory := command.NewFactory(envRepository)

	return step.NewXcodebuildArchiver(xcodeVersionProvider, inputParser, pathProvider, pathChecker, pathModifier, fileManager, logger, cmdFactory)
}

func createRunOptions(config step.Config) step.RunOpts {
	return step.RunOpts{
		ProjectPath:       config.ProjectPath,
		Scheme:            config.Scheme,
		Configuration:     config.Configuration,
		LogFormatter:      config.LogFormatter,
		XcodeMajorVersion: config.XcodeMajorVersion,
		ArtifactName:      config.ArtifactName,

		CodesignManager: config.CodesignManager,

		PerformCleanAction:          config.PerformCleanAction,
		XcconfigContent:             config.XcconfigContent,
		XcodebuildAdditionalOptions: config.XcodebuildAdditionalOptions,
		CacheLevel:                  config.CacheLevel,

		CustomExportOptionsPlistContent: config.ExportOptionsPlistContent,
		ExportMethod:                    config.ExportMethod,
		ICloudContainerEnvironment:      config.ICloudContainerEnvironment,
		ExportDevelopmentTeam:           config.ExportDevelopmentTeam,
		UploadBitcode:                   config.UploadBitcode,
		CompileBitcode:                  config.CompileBitcode,
	}
}

func createExportOptions(config step.Config, result step.RunResult) step.ExportOpts {
	return step.ExportOpts{
		OutputDir:      config.OutputDir,
		ArtifactName:   result.ArtifactName,
		ExportAllDsyms: config.ExportAllDsyms,

		Archive: result.Archive,

		ExportOptionsPath: result.ExportOptionsPath,
		IPAExportDir:      result.IPAExportDir,

		XcodebuildArchiveLog:       result.XcodebuildArchiveLog,
		XcodebuildExportArchiveLog: result.XcodebuildExportArchiveLog,
		IDEDistrubutionLogsDir:     result.IDEDistrubutionLogsDir,
	}
}
