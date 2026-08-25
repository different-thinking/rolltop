# OIDC Sign-In Plugin

Adds OpenID Connect sign-in for Rolltop users. Enable the plugin in Admin settings, then configure the provider with environment variables.

## Environment

```sh
ROLLTOP_OIDC_ISSUER=https://issuer.example.com
ROLLTOP_OIDC_CLIENT_ID=rolltop
ROLLTOP_OIDC_CLIENT_SECRET=...
ROLLTOP_OIDC_REDIRECT_URL=https://mail.example.com/api/plugins/oidc/callback
ROLLTOP_OIDC_NAME=OIDC
ROLLTOP_OIDC_SCOPES="openid email profile"
ROLLTOP_OIDC_ALLOWED_DOMAINS=example.com
ROLLTOP_OIDC_ALLOWED_EMAILS=person@example.com
ROLLTOP_OIDC_AUTO_CREATE=false
ROLLTOP_OIDC_ALLOW_UNVERIFIED_EMAIL=false
```

`ROLLTOP_OIDC_REDIRECT_URL` is optional when the app's public origin is known. The plugin derives `/api/plugins/oidc/callback` against `ROLLTOP_PUBLIC_URL` when that is set (recommended — it is not client-controllable), and otherwise against the request's `X-Forwarded-Host`/`X-Forwarded-Proto`. Set `ROLLTOP_PUBLIC_URL` in any deployment where the incoming host header cannot be fully trusted, so a spoofed host cannot steer the OIDC redirect.

By default, OIDC sign-in only works for existing Rolltop users whose email matches the verified OIDC email claim. Set `ROLLTOP_OIDC_AUTO_CREATE=true` to create users automatically; the first auto-created user becomes admin.

Sign-in requires a positive `email_verified`. The plugin reads it from the ID token, and falls back to the userinfo endpoint when the token omits either the email or the verification status. An **absent** `email_verified` is treated as unverified and rejected, because an identity provider that lets a user set an arbitrary, unverified email could otherwise be used to sign in as another Rolltop user. If your provider never sends `email_verified` and you trust it to issue only verified addresses, set `ROLLTOP_OIDC_ALLOW_UNVERIFIED_EMAIL=true` to opt out of the check. This is a behavior change: providers that omit the claim previously signed in and now need this flag.
