package dap

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// statStartTimeIndex locates starttime, field 22 of /proc/<pid>/stat, among the
// fields that follow the command name (which is field 2).
const statStartTimeIndex = 22 - 3

// processStartTime reads the process's start time, in clock ticks since boot.
func processStartTime(pid int) (processStart, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return processStart{}, false
	}
	// The command name is parenthesized and may itself hold spaces or
	// parentheses, so the remaining fields start after the last ')'.
	nameEnd := bytes.LastIndexByte(stat, ')')
	if nameEnd < 0 {
		return processStart{}, false
	}
	fields := strings.Fields(string(stat[nameEnd+1:]))
	if len(fields) <= statStartTimeIndex {
		return processStart{}, false
	}
	ticks, err := strconv.ParseInt(fields[statStartTimeIndex], 10, 64)
	if err != nil {
		return processStart{}, false
	}
	return processStart{ticks, 0}, true
}
