//go:build unix

package opencodeacp

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

var (
	openCodeSyscallGetpgid = syscall.Getpgid
	openCodeSyscallKill    = syscall.Kill
	procReadFile           = os.ReadFile
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func terminateOpenCodeProcess(cmd *exec.Cmd) error {
	return signalOpenCodeProcessGroup(cmd, syscall.SIGTERM)
}

func killOpenCodeProcess(cmd *exec.Cmd) error {
	return signalOpenCodeProcessGroup(cmd, syscall.SIGKILL)
}

func signalOpenCodeProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := openCodeSyscallGetpgid(cmd.Process.Pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if err := openCodeSyscallKill(-pgid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}

	return nil
}

func killProcessID(pid int) error {
	if pid <= 0 {
		return nil
	}
	pgid, err := openCodeSyscallGetpgid(pid)
	if err == nil {
		pid = -pgid
	}
	if err := openCodeSyscallKill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}

func inspectOpenCodeProcess(pid int) (processIdentity, error) {
	if pid <= 0 {
		return processIdentity{}, syscall.ESRCH
	}
	stat, err := procReadFile(procPath(pid, "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	startTime, err := procStartTime(string(stat))
	if err != nil {
		return processIdentity{}, err
	}
	cmdlineData, err := procReadFile(procPath(pid, "cmdline"))
	if err != nil {
		return processIdentity{}, err
	}
	envData, err := procReadFile(procPath(pid, "environ"))
	if err != nil {
		return processIdentity{}, err
	}

	return processIdentity{
		StartTime: startTime,
		Cmdline:   splitProcNUL(cmdlineData),
		Env:       splitProcEnv(envData),
	}, nil
}

func procPath(pid int, name string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + name
}

func procStartTime(stat string) (string, error) {
	closeParen := strings.LastIndex(stat, ")")
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return "", errors.New("malformed proc stat")
	}
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) < 20 {
		return "", errors.New("proc stat missing start time")
	}
	return fields[19], nil
}

func splitProcNUL(data []byte) []string {
	raw := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func splitProcEnv(data []byte) map[string]string {
	env := map[string]string{}
	for _, entry := range splitProcNUL(data) {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}
