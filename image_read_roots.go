package opencodeacp

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// managedImageRoots freezes disjoint directory handles before the first native
// launch. The host keeps these read domains and the complete preparation domains
// disjoint for the Agent lifetime, including mount aliases and root replacement.
// Later sessions may derive a narrower root through one of these handles.
type managedImageRoots struct {
	mu         sync.RWMutex
	ready      bool
	closed     bool
	workspaces map[string]struct{}
	roots      map[string]managedImageRoot
}

type managedImageRoot struct {
	handle   *os.Root
	resolved string
}

var errManagedImageRoot = errors.New("image path has no disjoint managed read root")

func (a *Agent) rememberImageWorkspaces(cwd string, additional []string) {
	if !a.options.hostAuthorityConfigured {
		return
	}

	a.imageRoots.mu.Lock()
	defer a.imageRoots.mu.Unlock()

	if a.imageRoots.ready || a.imageRoots.closed {
		return
	}

	if a.imageRoots.workspaces == nil {
		a.imageRoots.workspaces = make(map[string]struct{})
	}

	a.imageRoots.workspaces[cwd] = struct{}{}
	for _, path := range additional {
		a.imageRoots.workspaces[path] = struct{}{}
	}
}

// prepareManagedImageRoots runs before any authority call. Scratch reserves all
// future generated native trees; Home reserves the configured native residence.
// The adjacent adapter control root is not itself a prepared native tree.
func (a *Agent) prepareManagedImageRoots() error {
	if !a.options.hostAuthorityConfigured {
		return nil
	}

	roots := &a.imageRoots
	roots.mu.Lock()
	defer roots.mu.Unlock()

	if roots.closed {
		return errManagedImageRoot
	}

	if roots.ready {
		return nil
	}

	domains := make([][]os.FileInfo, 0, 2)

	for _, path := range []string{a.options.Home, a.scratchParent()} {
		if path == "" {
			continue
		}

		if err := os.MkdirAll(path, 0o700); err != nil {
			return errManagedImageRoot
		}

		_, lineage, err := imageDirectoryLineage(path)
		if err != nil {
			return errManagedImageRoot
		}

		domains = append(domains, lineage)
	}

	roots.roots = make(map[string]managedImageRoot)
	for path := range roots.workspaces {
		roots.pin(path, domains)
	}

	roots.pin(a.options.InputHandoffRoot, domains)
	roots.workspaces = nil
	roots.ready = true

	return nil
}

func imageDirectoryLineage(path string) (string, []os.FileInfo, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}

	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", nil, err
	}

	var lineage []os.FileInfo

	for path := absolute; ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err != nil {
			return "", nil, err
		}

		lineage = append(lineage, info)
		if filepath.Dir(path) == path {
			return absolute, lineage, nil
		}
	}
}

func imageLineageContains(lineage []os.FileInfo, directory os.FileInfo) bool {
	for _, ancestor := range lineage {
		if os.SameFile(ancestor, directory) {
			return true
		}
	}

	return false
}

func (r *managedImageRoots) pin(path string, domains [][]os.FileInfo) {
	if path == "" {
		return
	}

	if _, exists := r.roots[path]; exists {
		return
	}

	resolved, lineage, err := imageDirectoryLineage(path)
	if err != nil || !lineage[0].IsDir() {
		return
	}

	for _, domain := range domains {
		if imageLineageContains(lineage, domain[0]) || imageLineageContains(domain, lineage[0]) {
			return
		}
	}

	handle, err := os.OpenRoot(resolved)
	if err != nil {
		return
	}

	info, err := handle.Stat(".")
	if err != nil || !os.SameFile(info, lineage[0]) {
		_ = handle.Close()

		return
	}

	r.roots[path] = managedImageRoot{handle: handle, resolved: resolved}
}

func (a *Agent) closeManagedImageRoots() {
	roots := &a.imageRoots
	roots.mu.Lock()
	defer roots.mu.Unlock()

	roots.closed = true
	for path, root := range roots.roots {
		_ = root.handle.Close()

		delete(roots.roots, path)
	}
}

// openManagedImage uses only handles captured before preparation. Neither a
// later native path nor a newly offered workspace is resolved in the ambient
// filesystem. requested limits each read to that session's authorized roots.
func (a *Agent) openManagedImage(path string, requested []string) (*os.File, error) {
	if err := a.prepareManagedImageRoots(); err != nil {
		return nil, err
	}

	roots := &a.imageRoots

	roots.mu.RLock()
	defer roots.mu.RUnlock()

	if roots.closed {
		return nil, errManagedImageRoot
	}

	for _, allowed := range requested {
		if pinned, exists := roots.roots[allowed]; exists {
			for _, base := range []string{allowed, pinned.resolved} {
				if pathWithinRoot(base, path) {
					name, err := filepath.Rel(base, path)
					if err == nil {
						return pinned.handle.OpenFile(name, os.O_RDONLY|handoffOpenFlags, 0)
					}
				}
			}
		}

		for original, pinned := range roots.roots {
			for _, base := range []string{original, pinned.resolved} {
				if !pathWithinRoot(base, allowed) || !pathWithinRoot(allowed, path) {
					continue
				}

				rootName, err := filepath.Rel(base, allowed)
				if err != nil {
					continue
				}

				name, err := filepath.Rel(allowed, path)
				if err != nil {
					continue
				}

				scope, err := pinned.handle.OpenRoot(rootName)
				if err != nil {
					return nil, err
				}

				file, err := scope.OpenFile(name, os.O_RDONLY|handoffOpenFlags, 0)
				_ = scope.Close()

				return file, err
			}
		}
	}

	return nil, errManagedImageRoot
}
