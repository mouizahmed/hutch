# Verify a box identity

Export and securely give your service the **public** output of:

```sh
hutch machine identity > hutch-identity.json
hutch config set keys.identity_hosts service.example.com
```

The optional [Node verifier](verify.mjs) uses Node's built-in crypto module. Pin
the issuer and public keys from that file, set the exact HTTPS hostname as the
audience, and provide a policy that authorizes a box and project:

```js
const claims = verifyBoxIdentity(request.headers['x-hutch-identity'], trusted, {
  audience: 'service.example.com',
  authorize: c => allowedBoxes.has(c.sub) && c.project === expectedProject,
  seen: recentTokenIDs,
});
```

`recentTokenIDs` is an optional `Map` that refuses repeated token IDs until
expiry and caps its entries. Multiple service instances need a shared replay
store if replay protection is required. Never load a key from an incoming JWT,
trust an unsigned decoded claim, or treat caller identity as authorization for
every operation. Keep the service's normal authentication and authorization.

Removing the host from Hutch stops new identities. Already issued identities
remain valid until their five-minute expiry. Requests must use the box's HTTPS
proxy; the verifier rejects requests that have no valid identity.
