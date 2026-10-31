import test from "node:test";
import assert from "node:assert/strict";
import { generateKeyPairSync, randomBytes, sign } from "node:crypto";
import { verifyBoxIdentity } from "../examples/identity/verify.mjs";

const { privateKey, publicKey } = generateKeyPairSync("ed25519");
const trusted = {
  issuer: "urn:hutch:fixture",
  keys: [
    {
      ...publicKey.export({ format: "jwk" }),
      kid: "fixture",
      alg: "EdDSA",
      use: "sig",
    },
  ],
};
const now = 1793448000;
const base = {
  iss: trusted.issuer,
  sub: "fixture-box",
  project: "fixture-project",
  aud: "identity.example",
  iat: now,
  nbf: now,
  exp: now + 300,
  jti: randomBytes(16).toString("base64url"),
};
const options = {
  audience: base.aud,
  now,
  authorize: (c) => c.sub === base.sub && c.project === base.project,
};
function token(claims = {}, header = {}) {
  const body = [
    JSON.stringify({ alg: "EdDSA", typ: "JWT", kid: "fixture", ...header }),
    JSON.stringify({ ...base, ...claims }),
  ]
    .map((x) => Buffer.from(x).toString("base64url"))
    .join(".");
  return `${body}.${sign(null, Buffer.from(body), privateKey).toString("base64url")}`;
}

test("independent verifier checks a signed identity and rejects replay when enabled", () => {
  const jwt = token();
  const seen = new Map();
  assert.equal(
    verifyBoxIdentity(jwt, trusted, { ...options, seen }).sub,
    base.sub,
  );
  assert.throws(
    () => verifyBoxIdentity(jwt, trusted, { ...options, seen }),
    /invalid Hutch identity/,
  );
});

test("valid signatures do not bypass claim or authorization checks", () => {
  for (const claims of [
    { iss: "other" },
    { aud: "other" },
    { sub: "other" },
    { project: "other" },
    { exp: now },
    { exp: now + 301 },
    { nbf: now + 60, iat: now + 60, exp: now + 120 },
    { exp: "tomorrow" },
    { jti: "" },
    { nbf: now - 1 },
  ])
    assert.throws(
      () => verifyBoxIdentity(token(claims), trusted, options),
      /invalid Hutch identity/,
    );
  assert.throws(() =>
    verifyBoxIdentity(token(), trusted, { ...options, authorize: () => false }),
  );
  assert.throws(
    () => verifyBoxIdentity(token(), trusted, { audience: base.aud }),
    /authorization policy/,
  );
});

test("untrusted algorithms, keys, signatures and malformed encodings are refused", () => {
  for (const header of [
    { alg: "none" },
    { alg: "HS256" },
    { kid: "other" },
    { crit: ["unsupported"] },
  ]) {
    assert.throws(() => verifyBoxIdentity(token({}, header), trusted, options));
  }
  const jwt = token();
  for (const malformed of [
    "",
    "a.b.c",
    jwt + "=",
    jwt.replace(/.$/, "."),
    "x".repeat(4097),
  ]) {
    assert.throws(() => verifyBoxIdentity(malformed, trusted, options));
  }
  const other = generateKeyPairSync("ed25519").publicKey.export({
    format: "jwk",
  });
  assert.throws(() =>
    verifyBoxIdentity(
      jwt,
      { ...trusted, keys: [{ ...trusted.keys[0], x: other.x }] },
      options,
    ),
  );
});
