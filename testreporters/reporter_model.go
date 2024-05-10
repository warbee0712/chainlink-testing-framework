package testreporters

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"

	"github.com/smartcontractkit/chainlink-testing-framework/k8s/environment"
)

type GrafanaURLProvider interface {
	GetGrafanaBaseURL() (string, error)
	GetGrafanaDashboardURL() (string, error)
}

// TestReporter is a general interface for all test reporters
type TestReporter interface {
	WriteReport(folderLocation string) error
	SendSlackNotification(t *testing.T, slackClient *slack.Client, grafanaUrlProvider GrafanaURLProvider) error
	SetNamespace(namespace string)
}

const (
	// DefaultArtifactsDir default artifacts dir
	DefaultArtifactsDir string = "logs"
)

// WriteTeardownLogs attempts to download the logs of all ephemeral test deployments onto the test runner, also writing
// a test report if one is provided. A failing log level also enables you to fail a test based on what level logs the
// Chainlink nodes have thrown during their test.
func WriteTeardownLogs(
	t *testing.T,
	env *environment.Environment,
	optionalTestReporter TestReporter,
	failingLogLevel zapcore.Level, // Chainlink core uses zapcore for logging https://docs.chain.link/chainlink-nodes/v1/configuration#log_level
	grafanaUrlProvider GrafanaURLProvider,
) error {
	logsPath := filepath.Join(DefaultArtifactsDir, fmt.Sprintf("%s-%s-%d", t.Name(), env.Cfg.Namespace, time.Now().Unix()))
	if err := env.Artifacts.DumpTestResult(logsPath, "chainlink"); err != nil {
		log.Warn().Err(err).Msg("Error trying to collect pod logs")
		return err
	}
	logFiles, err := FindAllLogFilesToScan(logsPath, "node.log")
	if err != nil {
		log.Warn().Err(err).Msg("Error looking for pod logs")
		return err
	}
	verifyLogsGroup := &errgroup.Group{}
	for _, f := range logFiles {
		file := f
		verifyLogsGroup.Go(func() error {
			return VerifyLogFile(file, failingLogLevel, 1)
		})
	}
	assert.NoError(t, verifyLogsGroup.Wait(), "Found a concerning log")

	if t.Failed() || optionalTestReporter != nil {
		if err := SendReport(t, env.Cfg.Namespace, logsPath, optionalTestReporter, grafanaUrlProvider); err != nil {
			log.Warn().Err(err).Msg("Error writing test report")
		}
	}
	return nil
}

// SendReport writes a test report and sends a Slack notification if the test provides one
func SendReport(t *testing.T, namespace string, logsPath string, optionalTestReporter TestReporter, grafanaUrlProvider GrafanaURLProvider) error {
	if optionalTestReporter != nil {
		log.Info().Msg("Writing Test Report")
		optionalTestReporter.SetNamespace(namespace)
		err := optionalTestReporter.WriteReport(logsPath)
		if err != nil {
			return err
		}
		err = optionalTestReporter.SendSlackNotification(t, nil, grafanaUrlProvider)
		if err != nil {
			return err
		}
	}
	return nil
}

// FindAllLogFilesToScan walks through log files pulled from all pods, and gets all chainlink node logs
func FindAllLogFilesToScan(directoryPath string, partialFilename string) (logFilesToScan []*os.File, err error) {
	logFilePaths := []string{}
	err = filepath.Walk(directoryPath, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			logFilePaths = append(logFilePaths, path)
		}
		return nil
	})

	for _, filePath := range logFilePaths {
		if strings.Contains(filePath, partialFilename) {
			logFileToScan, err := os.Open(filePath)
			if err != nil {
				return nil, err
			}
			logFilesToScan = append(logFilesToScan, logFileToScan)
		}
	}
	return logFilesToScan, err
}

type WarnAboutAllowedMsgs = bool

const (
	WarnAboutAllowedMsgs_Yes WarnAboutAllowedMsgs = true
	WarnAboutAllowedMsgs_No  WarnAboutAllowedMsgs = false
)

// AllowedLogMessage is a log message that might be thrown by a Chainlink node during a test, but is not a concern
type AllowedLogMessage struct {
	message      string
	reason       string
	level        zapcore.Level
	logWhenFound WarnAboutAllowedMsgs
}

// NewAllowedLogMessage creates a new AllowedLogMessage. If logWhenFound is true, the log message will be printed to the
// console when found in the log file with Warn level (this can get noisy).
func NewAllowedLogMessage(message string, reason string, level zapcore.Level, logWhenFound WarnAboutAllowedMsgs) AllowedLogMessage {
	return AllowedLogMessage{
		message:      message,
		reason:       reason,
		level:        level,
		logWhenFound: logWhenFound,
	}
}

var defaultAllowedLogMessages = []AllowedLogMessage{
	{
		message: "No EVM primary nodes available: 0/1 nodes are alive",
		reason:  "Sometimes geth gets unlucky in the start up process and the Chainlink node starts before geth is ready",
		level:   zapcore.DPanicLevel,
	},
}

// VerifyLogFile verifies that a log file does not contain any logs at a level higher than the failingLogLevel. If it does,
// it will return an error. It also allows for a list of AllowedLogMessages to be passed in, which will be ignored if found
// in the log file. The failureThreshold is the number of logs at the failingLogLevel or higher that can be found before
// the function returns an error.
func VerifyLogFile(file *os.File, failingLogLevel zapcore.Level, failureThreshold uint, allowedMessages ...AllowedLogMessage) error {
	// nolint
	defer file.Close()

	var (
		zapLevel zapcore.Level
		err      error
	)
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)

	allAllowedMessages := append(defaultAllowedLogMessages, allowedMessages...)

	var logsFound uint

SCANNER_LOOP:
	for scanner.Scan() {
		jsonLogLine := scanner.Text()
		if !strings.HasPrefix(jsonLogLine, "{") { // don't bother with non-json lines
			if strings.HasPrefix(jsonLogLine, "panic") { // unless it's a panic
				return fmt.Errorf("found panic: %s", jsonLogLine)
			}
			continue
		}
		jsonMapping := map[string]any{}

		if err = json.Unmarshal([]byte(jsonLogLine), &jsonMapping); err != nil {
			// This error can occur anytime someone uses %+v in a log message, ignoring
			continue
		}
		logLevel, ok := jsonMapping["level"].(string)
		if !ok {
			return fmt.Errorf("found no log level in chainlink log line: %s", jsonLogLine)
		}

		if logLevel == "crit" { // "crit" is a custom core type they map to DPanic
			zapLevel = zapcore.DPanicLevel
		} else {
			zapLevel, err = zapcore.ParseLevel(logLevel)
			if err != nil {
				return fmt.Errorf("'%s' not a valid zapcore level", logLevel)
			}
		}

		if zapLevel >= failingLogLevel {
			logErr := fmt.Errorf("found log at level '%s', failing any log level higher than %s: %s", logLevel, zapLevel.String(), jsonLogLine)
			if failureThreshold > 1 {
				logErr = fmt.Errorf("found too many logs at level '%s' or above; failure threshold of %d reached; last error found: %s", logLevel, failureThreshold, jsonLogLine)
			}
			logMessage, hasMessage := jsonMapping["msg"]
			if !hasMessage {
				logsFound++
				if logsFound >= failureThreshold {
					return logErr
				}
				continue
			}

			for _, allowedLog := range allAllowedMessages {
				if strings.Contains(logMessage.(string), allowedLog.message) {
					if allowedLog.logWhenFound {
						log.Warn().
							Str("Reason", allowedLog.reason).
							Str("Level", allowedLog.level.CapitalString()).
							Str("Msg", logMessage.(string)).
							Msg("Found allowed log message, ignoring")
					}

					continue SCANNER_LOOP
				}
			}

			logsFound++
			if logsFound >= failureThreshold {
				return logErr
			}
		}
	}
	return nil
}
