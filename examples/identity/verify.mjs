// A service-side Hutch identity verifier using only Node's crypto module.
// Pin the output of `hutch machine identity`; never load a key from the JWT.
import { createPublicKey, verify } from "node:crypto";

function invalid() {
  throw new Error("invalid Hutch identity");
}
function decode(part) {
  if (!/^[A-Za-z0-9_-]+$/.test(part)) invalid();
  const bytes = Buffer.from(part, "base64url");
  if (bytes.toString("base64url") !== part) invalid();
  return bytes;
}

export function verifyBoxIdentity(
  token,
  trusted,
  { audience, authorize, now = Math.floor(Date.now() / 1000), seen } = {},
) {
  if (
    typeof audience !== "string" ||
    !audience ||
    typeof authorize !== "function"
  ) {
    throw new Error("configure an audience and an authorization policy");
  }
  try {
    if (typeof token !== "string" || token.length > 4096) invalid();
    const parts = token.split(".");
    if (parts.length !== 3) invalid();
    const header = JSON.parse(
      new TextDecoder("utf-8", { fatal: true }).decode(decode(parts[0])),
    );
    const claims = JSON.parse(
      new TextDecoder("utf-8", { fatal: true }).decode(decode(parts[1])),
    );
    const signature = decode(parts[2]);
    if (
      !header ||
      header.alg !== "EdDSA" ||
      header.typ !== "JWT" ||
      typeof header.kid !== "string" ||
      header.crit !== undefined ||
      signature.length !== 64
    )
      invalid();
    const jwk = trusted.keys.find((key) => key.kid === header.kid);
    if (
      !jwk ||
      jwk.kty !== "OKP" ||
      jwk.crv !== "Ed25519" ||
      jwk.alg !== "EdDSA" ||
      jwk.use !== "sig"
    )
      invalid();
    const key = createPublicKey({ key: jwk, format: "jwk" });
    if (!verify(null, Buffer.from(`${parts[0]}.${parts[1]}`), key, signature))
      invalid();
    if (
      !claims ||
      claims.iss !== trusted.issuer ||
      claims.aud !== audience ||
      typeof claims.sub !== "string" ||
      !claims.sub ||
      typeof claims.project !== "string"
    )
      invalid();
    for (const field of ["iat", "nbf", "exp"])
      if (!Number.isSafeInteger(claims[field])) invalid();
    if (
      claims.nbf !== claims.iat ||
      claims.iat > now + 5 ||
      claims.exp <= now ||
      claims.exp <= claims.iat ||
      claims.exp - claims.iat > 300
    )
      invalid();
    if (
      typeof claims.jti !== "string" ||
      !/^[A-Za-z0-9_-]{22}$/.test(claims.jti)
    )
      invalid();
    if (authorize(claims) !== true) invalid();
    if (seen) {
      for (const [id, expiry] of seen) if (expiry <= now) seen.delete(id);
      if (seen.has(claims.jti) || seen.size >= 10000) invalid();
      seen.set(claims.jti, claims.exp);
    }
    return claims;
  } catch {
    invalid();
  }
}
