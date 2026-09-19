import { mkdirSync, writeFileSync } from "node:fs"
import { createHash } from "node:crypto"
import { join } from "node:path"

const proofRoot = __PROOF_ROOT__
const ownedKeys = __OWNED_KEYS__
const carrierKey = "acp-go-opencode"

export const SessionEnvironment = async ({ client, directory }) => {
  mkdirSync(proofRoot, { recursive: true, mode: 0o700 })
  writeFileSync(join(proofRoot, createHash("sha256").update(directory).digest("hex")), directory, { mode: 0o600 })
  return {
    "shell.env": async (input, output) => {
      if (!input.sessionID) return
      let id = input.sessionID
      const seen = new Set()
      let carrier
      while (id && !seen.has(id) && seen.size < 128) {
        seen.add(id)
        const response = await client.session.get({ path: { id }, query: { directory: input.cwd ?? directory } })
        if (response.error || !response.data) throw Error("session environment lookup failed")
        carrier = response.data.metadata?.[carrierKey]
        if (carrier) break
        id = response.data.parentID
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
