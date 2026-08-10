package opencodeacp

import (
	"maps"
	"slices"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

// sessionCarrier is the environment and search-path prefix one ACP session
// holds. It travels with the session rather than with the runtime: one native
// process serves every session of an Agent, so anything pinned to the process
// would be the same value for all of them, and a rotated bearer would reach the
// wrong operation. Both halves are written onto the addressed native session
// and reach only that session's shell boundary.
type sessionCarrier struct {
	Env           map[string]string
	ExtraPathDirs []string
}

func newSessionCarrier(env map[string]string, extraPathDirs []string) sessionCarrier {
	return sessionCarrier{
		Env:           cloneStringMap(env),
		ExtraPathDirs: append([]string{}, extraPathDirs...),
	}
}

// clone hands out a copy every caller may keep: the carrier crosses the session
// lock into runtime recovery and into the durable snapshot, and a shared map
// there would let one of them observe a rebind half applied.
func (c sessionCarrier) clone() sessionCarrier {
	return sessionCarrier{
		Env:           cloneStringMap(c.Env),
		ExtraPathDirs: append([]string{}, c.ExtraPathDirs...),
	}
}

func (c sessionCarrier) equal(other sessionCarrier) bool {
	return maps.Equal(c.Env, other.Env) && slices.Equal(c.ExtraPathDirs, other.ExtraPathDirs)
}

// scopeOptions renders the carrier for the native directory scope that owns the
// addressed session.
func (c sessionCarrier) scopeOptions(cwd string, mcpServers []opencode.MCPServerConfig) opencode.ScopeOptions {
	return opencode.ScopeOptions{
		Directory:     cwd,
		MCPServers:    mcpServers,
		Env:           cloneStringMap(c.Env),
		ExtraPathDirs: append([]string{}, c.ExtraPathDirs...),
	}
}

// carrierFromMeta resolves the carrier a lifecycle request asks for against the
// one already recorded for the session. An omitted half keeps the recorded
// value; a present half replaces it outright, including with an empty one, so
// clearing an operation's environment is expressible.
func carrierFromMeta(meta sessionMeta, recorded sessionCarrier) sessionCarrier {
	carrier := recorded.clone()

	if meta.EnvSet {
		carrier.Env = cloneStringMap(meta.Env)
	}

	if meta.ExtraPathDirsSet {
		carrier.ExtraPathDirs = append([]string{}, meta.ExtraPathDirs...)
	}

	return carrier
}
