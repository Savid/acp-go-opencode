import { mkdirSync, writeFileSync } from "node:fs"
import { createHash } from "node:crypto"
import { join } from "node:path"

const proofRoot = __PROOF_ROOT__
const ownedKeys = __OWNED_KEYS__
const carrierKey = "acp-go-opencode"

export const SessionEnvironment = async ({ client, directory }) => {
  mkdirSync(proofRoot, { recursive: true, mode: 0o700 })
  writeFileSync(join(proofRoot, createHash("sha256").update(directory).digest("hex")), directory, { mode: 0o600 })
  // Sessions this instance has looked up: their immutable parent and current
  // carrier. Session updates refresh an entry and deletion drops it, so a
  // shell spawn reads the native API only for a session it has not seen.
  const sessions = new Map()
  const remember = (info) => {
    const entry = { parentID: info.parentID, carrier: info.metadata?.[carrierKey] }
    sessions.set(info.id, entry)
    return entry
  }
  return {
    event: async ({ event }) => {
      const info = event.properties?.info
      if (!info?.id || !sessions.has(info.id)) return
      if (event.type === "session.updated") remember(info)
      else if (event.type === "session.deleted") sessions.delete(info.id)
    },
    "shell.env": async (input, output) => {
      if (!input.sessionID) return
      let id = input.sessionID
      const seen = new Set()
      let carrier
      while (id && !seen.has(id) && seen.size < 128) {
        seen.add(id)
        let entry = sessions.get(id)
        if (!entry) {
          const response = await client.session.get({ path: { id }, query: { directory: input.cwd ?? directory } })
          if (response.error || !response.data) throw Error("session environment lookup failed")
          entry = remember(response.data)
        }
        carrier = entry.carrier
        if (carrier) break
        id = entry.parentID
      }
      if (!carrier || !carrier.env || !Array.isArray(carrier.extraPathDirs)) throw Error("session environment missing")
      for (const [key, value] of Object.entries(carrier.env)) {
        if (typeof value !== "string") throw Error("invalid session environment")
        if (!key.startsWith("ACP_GO_OPENCODE_INTERNAL_")) output.env[key] = value
      }
      for (const key of ownedKeys) {
        if (process.env[key] === undefined) delete output.env[key]
        else output.env[key] = process.env[key]
      }
      const base = carrier.env.PATH ?? output.env.PATH ?? process.env.PATH ?? ""
      output.env.PATH = [...carrier.extraPathDirs, ...base.split(":").filter(Boolean)].join(":")
    },
  }
}
