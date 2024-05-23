package modgearman

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

type queue struct {
	Name        string // queue names
	Total       int    // total number of jobs
	Running     int    // number of running jobs
	Waiting     int    // number of waiting jobs
	AvailWorker int    // total number of available worker
}

func getGearmanServerData(hostname string, port int) ([]queue, error) {
	var queueList []queue
	gearmanStatus, err := sendCmd2gearmandAdmin("status\nversion\n", hostname, port)

	if err != nil {
		//log.Errorf("%s", err)
		return []queue{}, err
	}

	if gearmanStatus == "" {
		return queueList, nil
	}

	// Organize queues into a list
	lines := strings.Split(gearmanStatus, "\n")
	for _, line := range lines {
		parts := strings.Fields(line)

		if len(parts) < 4 || (parts[0] == "dummy" && parts[1] == "") {
			continue
		}
		totalInt, err := strconv.Atoi(parts[1])
		if err != nil {
			err := fmt.Errorf("the recieved data is not in the right format: %s", err)
			return []queue{}, err
		}
		runningInt, err := strconv.Atoi(parts[2])
		if err != nil {
			err := fmt.Errorf("the recieved data is not in the right format: %s", err)
			return []queue{}, err
		}
		availWorkerInt, err := strconv.Atoi(parts[3])
		if err != nil {
			err := fmt.Errorf("the recieved data is not in the right format: %s", err)
			return []queue{}, err
		}

		queueList = append(queueList, queue{
			Name:        parts[0],
			Total:       totalInt,
			Running:     runningInt,
			AvailWorker: availWorkerInt,
			Waiting:     totalInt - runningInt,
		})
	}

	return queueList, nil
}

func sendCmd2gearmandAdmin(cmd string, hostname string, port int) (string, error) {
	addr := hostname + ":" + strconv.Itoa(port)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	_, writeErr := conn.Write([]byte(cmd))
	if writeErr != nil {
		return "", writeErr
	}

	// Read response
	var buffer bytes.Buffer
	tmp := make([]byte, 4000)

	for {
		n, readErr := conn.Read(tmp)
		if n > 0 {
			buffer.Write(tmp[:n])
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", readErr
		}
		if n > 0 && tmp[n-1] == '\n' {
			break
		}
	}
	return buffer.String(), nil
}