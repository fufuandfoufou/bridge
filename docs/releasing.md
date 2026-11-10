# Releasing

Pushing a tag such as `v0.1.0` runs `.github/workflows/release.yml`: it tests, builds a universal app, signs it with a Developer ID, notarizes the dmg, and attaches it to a GitHub Release. Without the signing secrets it only runs the tests and skips publishing, so it never publishes an unsigned build.

## One-time setup

You need a paid [Apple Developer Program](https://developer.apple.com/programs/) membership.

1. **Certificate.** In Xcode, open Settings → Accounts → Manage Certificates and add a *Developer ID Application* certificate. Export it from Keychain Access as a `.p12` with a password.
2. **Notary key.** In App Store Connect, open Users and Access → Integrations → App Store Connect API and create a key with the *Developer* role. Download the `.p8` once, and note the Key ID and Issuer ID.
3. **Secrets.** Add these to the repository:

```bash
base64 -i cert.p12 | gh secret set MACOS_CERT_P12_BASE64
gh secret set MACOS_CERT_PASSWORD
gh secret set MACOS_SIGN_IDENTITY --body "Developer ID Application: Your Name (TEAMID)"
base64 -i AuthKey_XXXX.p8 | gh secret set APPLE_NOTARY_KEY_BASE64
gh secret set APPLE_NOTARY_KEY_ID --body "XXXX"
gh secret set APPLE_NOTARY_ISSUER_ID --body "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
```

## Publishing

```bash
git tag v0.1.0
git push origin v0.1.0
```
