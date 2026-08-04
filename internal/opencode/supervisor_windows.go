//go:build windows

package opencode

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type guardianContainment struct {
	job  windows.Handle
	name string
}

type livenessContainment struct {
	job    windows.Handle
	waiter *supervisorWaiter
}

type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func newGuardianContainment(supervisorConfig) (*guardianContainment, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("create Windows Job Object nonce: %w", err)
	}
	name := `Local\acp-go-opencode-runtime-` + hex.EncodeToString(nonce[:])
	job, err := createRuntimeJob(name)
	if err != nil {
		return nil, err
	}
	return &guardianContainment{job: job, name: name}, nil
}

func createRuntimeJob(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("encode Windows Job Object name: %w", err)
	}
	job, err := windows.CreateJobObject(nil, namePtr)
	if err != nil && job == 0 {
		return 0, fmt.Errorf("create Windows Job Object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("set Windows Job Object kill-on-close: %w", err)
	}
	return job, nil
}

func (c *guardianContainment) Name() string { return c.name }

func (c *guardianContainment) Close() error {
	return windows.CloseHandle(c.job)
}

func (c *guardianContainment) Quiesce(_ int, timeout time.Duration) error {
	return quiesceJob(c.job, timeout)
}

func openLivenessContainment(config supervisorConfig) (*livenessContainment, error) {
	name := config.JobName
	if name == "" {
		return nil, errors.New("Windows Job Object name is required")
	}
	job, err := createRuntimeJob(name)
	if err != nil {
		return nil, err
	}
	return &livenessContainment{job: job}, nil
}

func (c *livenessContainment) Start(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED,
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	c.waiter = newSupervisorWaiter(cmd, true)

	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION,
		false,
		uint32(cmd.Process.Pid),
	)
	if err == nil {
		err = windows.AssignProcessToJobObject(c.job, process)
		windows.CloseHandle(process)
	}
	if err == nil {
		err = resumePrimaryThread(uint32(cmd.Process.Pid))
	}
	if err != nil {
		_ = cmd.Process.Kill()
		c.waiter.start()
		<-c.waiter.result()

		return fmt.Errorf("assign suspended native root to Windows Job Object: %w", err)
	}

	c.waiter.start()

	return nil
}

func (c *livenessContainment) Wait() <-chan error { return c.waiter.result() }

func resumePrimaryThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return openErr
			}
			_, resumeErr := windows.ResumeThread(thread)
			windows.CloseHandle(thread)
			return resumeErr
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ThreadEntry32{}))
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
				return fmt.Errorf("primary thread for process %d was not found", pid)
			}
			return err
		}
	}
}

func (c *livenessContainment) Close() error {
	return windows.CloseHandle(c.job)
}

func (c *livenessContainment) Quiesce(_ int, timeout time.Duration) error {
	return quiesceJob(c.job, timeout)
}

func quiesceJob(job windows.Handle, timeout time.Duration) error {
	active, err := activeJobProcesses(job)
	if err != nil {
		return err
	}
	if active > 0 {
		if err := windows.TerminateJobObject(job, 1); err != nil {
			return fmt.Errorf("terminate Windows Job Object: %w", err)
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		active, err = activeJobProcesses(job)
		if err != nil {
			return err
		}
		if active == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("Windows Job Object retained %d active processes", active)
}

func activeJobProcesses(job windows.Handle) (uint32, error) {
	info := jobBasicAccounting{}
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		return 0, fmt.Errorf("query Windows Job Object process count: %w", err)
	}
	return info.ActiveProcesses, nil
}

func querySupervisorProcessSnapshot(identityPath string) (int, bool) {
	raw, err := os.ReadFile(identityPath)
	if err != nil {
		return 0, false
	}

	name := strings.TrimSpace(string(raw))
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil || name == "" {
		return 0, false
	}

	job, openErr := windows.CreateJobObject(nil, namePtr)
	if job == 0 || !errors.Is(openErr, windows.ERROR_ALREADY_EXISTS) {
		if job != 0 {
			_ = windows.CloseHandle(job)
		}

		return 0, false
	}
	defer windows.CloseHandle(job) //nolint:errcheck // The queried count remains valid after best-effort handle cleanup.

	active, err := activeJobProcesses(job)
	if err != nil {
		return 0, false
	}

	return int(active), true
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func startIndependentSupervisor(cmd *exec.Cmd) error {
	return cmd.Start()
}

func releaseIndependentSupervisorWaiter(cmd *exec.Cmd, waiter *supervisorWaiter) (int, error) {
	if cmd == nil || cmd.Process == nil || waiter == nil {
		return 0, errors.Join(ErrProcessContainmentIncomplete, errors.New("direct-child waiter is unavailable"))
	}

	waiter.start()

	return cmd.Process.Pid, nil
}
