package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

func validateHostAuthority(authority HostAuthority) (err error) {
	_, err = hostAuthorityEnvironment(authority)

	return err
}

func hostAuthorityEnvironment(authority HostAuthority) (environment map[string]string, err error) {
	if authority == nil {
		return nil, ErrHostAuthorityUnavailable
	}

	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
			environment = nil
		}
	}()

	environment = authority.NativeEnvironment()
	if environment == nil {
		return nil, ErrHostAuthorityUnavailable
	}

	if err := opencode.ValidateEnvironment(environment); err != nil {
		return nil, errors.Join(ErrHostAuthorityUnavailable, err)
	}

	return cloneStringMap(environment), nil
}

func authorityProcessStarter(authority HostAuthority) opencode.ProcessStarter {
	return func(ctx context.Context, executable string, arguments, environment []string, workingDirectory string) (handle opencode.ProcessHandle, err error) {
		defer func() {
			if recover() != nil {
				handle = opencode.ProcessHandle{}
				err = ErrHostAuthorityUnavailable
			}
		}()

		process, err := authority.StartNative(ctx, NativeRequest{
			Executable:       executable,
			Arguments:        append([]string(nil), arguments...),
			Environment:      append([]string(nil), environment...),
			WorkingDirectory: workingDirectory,
		})
		if err != nil {
			return opencode.ProcessHandle{}, opencode.MarkProcessStartSettled(err)
		}

		if nativeProcessNil(process) {
			return opencode.ProcessHandle{}, errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete)
		}

		stdin, stdout, stderr, stdioErr := nativeProcessStdio(process)
		if stdioErr != nil {
			return opencode.ProcessHandle{}, errors.Join(stdioErr, settleUnusableNativeProcess(process))
		}

		if stdin == nil || stdout == nil || stderr == nil {
			return opencode.ProcessHandle{}, errors.Join(
				errors.New("native process returned unusable host stdio"), settleUnusableNativeProcess(process),
			)
		}

		return opencode.ProcessHandle{
			Input: stdin, Output: stdout, Errors: stderr,
			Await: func(waitCtx context.Context) (outcome opencode.ProcessOutcome, waitErr error) {
				defer func() {
					if recover() != nil {
						outcome = opencode.ProcessOutcome{}
						waitErr = errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete)
					}
				}()

				result, waitErr := process.Wait(waitCtx)

				outcome = opencode.ProcessOutcome{
					ExitCode: result.ExitCode,
					Signal:   result.Signal,
					Revoked:  result.Revoked,
				}

				if waitErr != nil {
					ctxErr := waitCtx.Err()
					if ctxErr == nil || !errors.Is(waitErr, ctxErr) {
						waitErr = errors.Join(ErrContainmentIncomplete, waitErr)
					}
				}

				return outcome, waitErr
			},
			Stop: func(revokeCtx context.Context) (revokeErr error) {
				defer func() {
					if recover() != nil {
						revokeErr = errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete)
					}
				}()

				return process.Revoke(revokeCtx)
			},
		}, nil
	}
}

func nativeProcessNil(process NativeProcess) bool {
	if process == nil {
		return true
	}

	value := reflect.ValueOf(process)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func nativeProcessStdio(process NativeProcess) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stdin, stdout, stderr = nil, nil, nil
			err = errors.Join(ErrHostAuthorityUnavailable, fmt.Errorf("native process stdio panicked: %v", recovered))
		}
	}()

	return process.Stdin(), process.Stdout(), process.Stderr(), nil
}

func settleUnusableNativeProcess(process NativeProcess) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	revokeErr := func() (err error) {
		defer func() {
			if recover() != nil {
				err = ErrHostAuthorityUnavailable
			}
		}()

		return process.Revoke(ctx)
	}()

	_, waitErr := func() (result NativeResult, err error) {
		defer func() {
			if recover() != nil {
				err = ErrHostAuthorityUnavailable
			}
		}()

		return process.Wait(ctx)
	}()
	if waitErr != nil {
		return errors.Join(revokeErr, ErrContainmentIncomplete, waitErr)
	}

	return opencode.MarkProcessStartSettled(errors.Join(errors.New("native process settled after unusable stdio"), revokeErr))
}
