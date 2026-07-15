package homelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	ClaimFileName    = "acp-go-opencode-runtime.claim.lock"
	LivenessFileName = "acp-go-opencode-runtime.liveness.lock"
)

var chmodLockFile = (*os.File).Chmod
var lockPlatform = platformLock
var verifyLockPath = verifyLockedPath
var validateFS = validateLockFilesystem

type Lock struct {
	files []*os.File
	once  sync.Once
	err   error
}

func Acquire(home string) (*Lock, error) {
	claim, err := AcquireClaim(home)
	if err != nil {
		return nil, err
	}

	liveness, err := AcquireLiveness(home)
	if err != nil {
		_ = claim.Release()

		return nil, err
	}

	return &Lock{files: append(claim.files, liveness.files...)}, nil
}

func AcquireClaim(home string) (*Lock, error) {
	return acquire(home, ClaimFileName, "claim OpenCode writable home")
}

func AcquireLiveness(home string) (*Lock, error) {
	return acquire(home, LivenessFileName, "claim OpenCode home liveness")
}

func acquire(home string, name string, action string) (*Lock, error) {
	if home == "" {
		return nil, errors.New("OpenCode writable home is required for runtime exclusivity")
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create OpenCode home for runtime lock: %w", err)
	}

	path := filepath.Join(home, name)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime lock %s: %w", name, err)
	}

	if err := chmodLockFile(file, 0o600); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("chmod runtime lock %s: %w", name, err)
	}

	if err := validateFS(file); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("validate OpenCode writable-home filesystem: %w", err)
	}

	if err := lockPlatform(file); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("%s: %w", action, err)
	}

	if err := verifyLockPath(file, path); err != nil {
		_ = platformUnlock(file)
		_ = file.Close()

		return nil, err
	}

	return &Lock{files: []*os.File{file}}, nil
}

func verifyLockedPath(file *os.File, path string) error {
	held, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat held runtime lock: %w", err)
	}

	current, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat runtime lock path: %w", err)
	}

	if !os.SameFile(held, current) {
		return errors.New("runtime lock path changed during acquisition")
	}

	return nil
}

func (l *Lock) Release() error {
	if l == nil {
		return nil
	}

	l.once.Do(func() {
		for index := len(l.files) - 1; index >= 0; index-- {
			file := l.files[index]
			l.err = errors.Join(l.err, platformUnlock(file), file.Close())
		}
	})

	return l.err
}
