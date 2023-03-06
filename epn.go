package modgearman

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// ePNStartTimeout is the amount of time we wait for the socket to appear
	ePNStartTimeout = 5 * time.Second

	// ePNStartRetryInterval is the interval at which the socket is checked
	ePNStartRetryInterval = 50 * time.Millisecond

	// ePNMaxRetries sets the amount of retries when connecting to epn server
	ePNMaxRetries = 15
)

var (
	ePNServerProcess *exec.Cmd
	ePNServerSocket  *os.File

	// ePNFilePrefix contains prefixes to look for explicit epn flag
	ePNFilePrefix = []string{"# nagios:", "# naemon:", "# icinga:"}

	fileUsesEPNCache = make(map[string]bool)

	ePNStarted *time.Time
)

func startEmbeddedPerl(config *configurationStruct) {
	ePNServerProcess = nil
	ePNServerSocket = nil
	if !config.enableEmbeddedPerl {
		return
	}
	now := time.Now()
	ePNStarted = &now
	logger.Debugf("starting embedded perl worker")
	args := make([]string, 0)
	if config.usePerlCache {
		args = append(args, "-c")
	}
	if config.debug >= LogLevelDebug {
		args = append(args, "-v")
	}
	if config.debug >= LogLevelTrace {
		args = append(args, "-vv")
	}
	socketPath, err := os.CreateTemp("", "mod_gearman_worker_epn*.socket")
	if err != nil {
		err = fmt.Errorf("failed to create epn socket: %w: %s", err, err.Error())
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		logger.Errorf("epn startup error: %s", err)
		cleanExit(ExitCodeError)
	}
	args = append(args, socketPath.Name())
	os.Remove(socketPath.Name())
	ePNServerSocket = socketPath

	cmd := exec.Command(config.p1File, args...)
	passthroughLogs("stdout", logger.Debugf, cmd.StdoutPipe)
	passthroughLogs("stderr", logger.Errorf, cmd.StderrPipe)

	err = cmd.Start()
	if err != nil {
		err = fmt.Errorf("failed to start epn worker: %w: %s", err, err.Error())
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		logger.Errorf("epn startup error: %s", err)
		cleanExit(ExitCodeError)
	}
	ePNServerProcess = cmd

	go func() {
		defer logPanicExit()
		err := cmd.Wait()
		if err != nil {
			logger.Errorf("epn server errored: %w: %s", err, err.Error())
		}
		stopEmbeddedPerl()
	}()

	// wait till socket appears
	ticker := time.NewTicker(ePNStartRetryInterval)
	timeout := time.NewTimer(ePNStartTimeout)
	keepTrying := true
	for keepTrying {
		select {
		case <-timeout.C:
			err = fmt.Errorf("timeout (%s) while waiting for epn socket", ePNStartTimeout)
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			logger.Errorf("epn startup error: %s", err)
			cleanExit(ExitCodeError)
		case <-ticker.C:
			_, err := os.Stat(socketPath.Name())
			if err == nil {
				keepTrying = false
			}
		}
	}
	ticker.Stop()
	timeout.Stop()
}

func stopEmbeddedPerl() {
	if ePNServerProcess == nil {
		return
	}
	if ePNServerProcess.Process == nil {
		return
	}

	ePNServerProcess.Process.Signal(os.Interrupt)
	ePNServerProcess.Process.Release()
	ePNServerProcess = nil
	if ePNServerSocket != nil {
		os.Remove(ePNServerSocket.Name())
	}
	ePNServerSocket = nil
	ePNStarted = nil
	logger.Debugf("epn worker shutdown complete")
}

func fileUsesEmbeddedPerl(file string, config *configurationStruct) bool {
	if !config.enableEmbeddedPerl {
		return false
	}
	cached, ok := fileUsesEPNCache[file]
	if ok {
		return cached
	}
	fileUsesEPN := detectFileUsesEmbeddedPerl(file, config)
	fileUsesEPNCache[file] = fileUsesEPN
	return fileUsesEPN
}

func detectFileUsesEmbeddedPerl(file string, config *configurationStruct) bool {
	readFile, err := os.Open(file)
	if err != nil {
		logger.Debugf("failed to open %s: %w: %s", file, err, err.Error())
		return false
	}
	defer readFile.Close()
	fileScanner := bufio.NewScanner(readFile)
	fileScanner.Split(bufio.ScanLines)
	linesRead := 0
	for linesRead < 10 && fileScanner.Scan() {
		line := fileScanner.Text()
		linesRead++
		if linesRead == 1 {
			// check if first line contains perl shebang
			if !strings.Contains(line, "/bin/perl") {
				return false
			}
			continue
		}
		if !strings.HasPrefix(line, "# ") {
			continue
		}
		for _, prefix := range ePNFilePrefix {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimPrefix(line, prefix)
				line = strings.TrimSpace(line)
				switch line[0:1] {
				case "+":
					return true
				case "-":
					return false
				}
			}
		}
	}

	// nothing explicitly found, fallback to config default
	return config.useEmbeddedPerlImplicitly
}

type ePNMsg struct {
	Bin     string            `json:"bin"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Timeout int               `json:"timeout"`
}

type ePNRes struct {
	RC              int     `json:"rc"`
	Stdout          string  `json:"stdout"`
	RunDuration     float64 `json:"run_duration"`
	CompileDuration float64 `json:"compile_duration"`
	CPUUser         float64 `json:"cpu_user"`
}

func executeWithEmbeddedPerl(cmd *command, result *answer, received *receivedStruct) error {
	msg, err := json.Marshal(ePNMsg{
		Bin:     cmd.Command,
		Args:    cmd.Args,
		Env:     cmd.Env,
		Timeout: received.timeout,
	})
	if err != nil {
		return fmt.Errorf("json error: %w: %s", err, err.Error())
	}
	msg = append(msg, '\n')

	c, err := ePNConnect()
	if err != nil {
		return fmt.Errorf("connecting to epn server failed: %w: %s", err, err.Error())
	}
	defer c.Close()

	received.Cancel = func() {
		logger.Errorf("closing epn conn")
		received.Canceled = true
		c.Close()
	}

	_, err = c.Write(msg)
	if err != nil {
		return fmt.Errorf("sending to epn server failed: %w: %s", err, err.Error())
	}

	buf, err := ePNReadResponse(c)
	if err != nil {
		return fmt.Errorf("reading epn response failed: %w: %s", err, err.Error())
	}

	received.Cancel = nil

	res := ePNRes{}
	err = json.Unmarshal(buf, &res)
	if err != nil {
		return fmt.Errorf("json unpacking failed: %w: %s", err, err.Error())
	}

	result.output = res.Stdout
	result.returnCode = res.RC
	result.compileDuration = res.CompileDuration
	result.runUserDuration = res.CPUUser

	return nil
}

func ePNConnect() (c net.Conn, err error) {
	retries := 0
	for {
		if !isRunning() {
			return nil, fmt.Errorf("worker is shuting down")
		}
		if ePNServerSocket == nil {
			time.Sleep(1 * time.Second)
			retries++
			if retries > ePNMaxRetries {
				return nil, fmt.Errorf("epn socket connect failed: no socket exists")
			}
			continue
		}

		s1 := ePNStarted
		c, err = net.Dial("unix", ePNServerSocket.Name())
		if err == nil {
			return c, nil
		}

		if retries == 0 {
			logger.Warnf("connecting to epn server failed (retry %d): %w: %s", retries, err, err.Error())
		} else {
			logger.Debugf("connecting to epn server failed (retry %d): %w: %s", retries, err, err.Error())
		}
		retries++

		if retries > ePNMaxRetries {
			return nil, fmt.Errorf("epn socket connect failed: %w: %s", err, err.Error())
		}

		time.Sleep(1 * time.Second)

		s2 := ePNStarted
		if retries%3 == 0 && s1 == s2 && s2 != nil && time.Now().After(s2.Add(ePNStartTimeout)) {
			// try restarting epn server
			logger.Debugf("restarting epn server")
			// retry connection to epn server
			ePNStarted = nil
		}
	}
}

// checkRestartEPNServer checks if epn server needs to be restarted
func checkRestartEPNServer(config *configurationStruct) {
	if !config.enableEmbeddedPerl {
		return
	}

	if ePNStarted != nil {
		return
	}

	now := time.Now()
	ePNStarted = &now

	logger.Warnf("restarting epn server")
	stopEmbeddedPerl()
	startEmbeddedPerl(config)
}

// read result from connection into result buffer with undefined result size
func ePNReadResponse(conn io.Reader) ([]byte, error) {
	body := new(bytes.Buffer)
	for {
		_, err := io.CopyN(body, conn, 65536)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("io.CopyN: %w: %s", err, err.Error())
			}
			break
		}
	}
	res := body.Bytes()
	return res, nil
}

// redirect log output from epn server to main worker log file
func passthroughLogs(name string, logFn func(f string, v ...interface{}), pipeFn func() (io.ReadCloser, error)) {
	pipe, err := pipeFn()
	if err != nil {
		err = fmt.Errorf("failed to connect to %s: %w: %s", name, err, err.Error())
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		logger.Errorf("epn startup error: %s", err)
		cleanExit(ExitCodeError)
	}
	read := bufio.NewReader(pipe)
	go func() {
		defer logPanicExit()
		for {
			line, _, err := read.ReadLine()
			if err != nil {
				break
			}
			if len(line) > 0 {
				logFn("%s", string(line))
			}
		}
	}()
}
