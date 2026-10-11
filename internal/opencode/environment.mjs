import { mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { createHash } from "node:crypto"
import { dirname, join } from "node:path"

const proofRoot = __PROOF_ROOT__
const ownedKeys = __OWNED_KEYS__
const carrierKey = "acp-go-opencode"

export const SessionEnvironment = async ({ client, directory }) => {
  mkdirSync(proofRoot, { recursive: true, mode: 0o700 })
  writeFileSync(join(proofRoot, createHash("sha256").update(directory).digest("hex")), directory, { mode: 0o600 })
  // Sessions this instance has looked up: their parent and carrier. The
  // adapter rewrites the generation file after it changes any carrier, and a
  // changed generation drops every entry, so a shell spawn reads the native
  // API only for a session it has not seen since.
  const root = dirname(proofRoot)
  const sessions = new Map()
  let generation
  return {
    "shell.env": async (input, output) => {
      if (!input.sessionID) return
      let current = ""
      try {
        current = readFileSync(join(root, "generation"), "utf8")
      } catch {}
      if (current !== generation) {
        generation = current
        sessions.clear()
      }
      let id = input.sessionID
      const seen = new Set()
      let carrier
      while (id && !seen.has(id) && seen.size < 128) {
        seen.add(id)
        let entry = sessions.get(id)
        if (!entry) {
          const response = await client.session.get({ path: { id }, query: { directory: input.cwd ?? directory } })
          if (response.error || !response.data) throw Error("session environment lookup failed")
          entry = { parentID: response.data.parentID, carrier: response.data.metadata?.[carrierKey] }
          if (current === generation) sessions.set(id, entry)
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
