# Passkeys

A passkey lets you sign in with your fingerprint, face or device PIN instead of your password and code. The passkey stays on your phone, your computer, a security key or your password manager; DockBack keeps only its **public key**, which can't be used to sign in anywhere.

A passkey has to **verify you** (the fingerprint, face or PIN, not just a tap) for DockBack to accept it, so on its own it counts as both the password and the two-factor code. It is also phishing-resistant: it works only on the site it was made for, so a look-alike page can't use it.

## Turn it on: `DOCKBACK_PUBLIC_URL`

Browsers keep passkeys to a site reached by **name**, over **https**. Tell DockBack the address you open it at:

```env
DOCKBACK_PUBLIC_URL=https://dockback.example.org
```

- It has to be a **name**, not an IP address: browsers refuse passkeys at an address like `10.0.0.5`.
- It has to be **https**. Plain `http` works only for `http://localhost`.
- Passkeys work only at **that** address. If you also open DockBack some other way (a LAN IP, a second name), sign in there with your password. The sign-in page offers a passkey only at the address passkeys belong to.
- A private name works: a Tailscale MagicDNS name with `tailscale serve`, or an internal domain with a certificate your devices trust. DockBack should stay off the public internet either way. See *HTTPS & TLS*.

Until it is set, Settings → Security → **Passkeys** says why passkeys are off.

## Add a passkey

**Settings → Security → Passkeys → Add a passkey.**

1. Confirm your password, plus your two-factor code if 2FA is on. Someone who finds your browser signed in can't add a passkey of their own.
2. Your browser or phone asks you to create the passkey with your fingerprint, face or PIN.
3. The passkey is named after the password manager that keeps it (iCloud Keychain, Google Password Manager, 1Password, Bitwarden, Windows Hello and others), or after your browser when it doesn't say. Rename it any time.

A **synced** badge means a password manager keeps the passkey and copies it to your other devices. Up to 20 passkeys per account.

## Sign in

On the sign-in page, choose **Sign in with a passkey**. There is no username to type: your device offers the passkeys it holds for DockBack. The button appears once someone has a passkey here.

Your password still works, so a lost phone doesn't lock you out. Remove that phone's passkey from Settings.

## How it is protected

- **Only public keys are stored**, together with a sign counter the authenticator updates on every use.
- **Every challenge works once**, within five minutes, and only for what it was made for: a sign-in challenge can't add a passkey, and the reverse.
- **A refused passkey counts against the same lockout as a wrong password** and raises the same alerts.
- **Everything is in the audit trail:** sign-ins (`login.ok` with `method=passkey`), refusals, and passkeys added, renamed or removed.
- **A passkey made for another address** (after you change `DOCKBACK_PUBLIC_URL`) is listed with a note. Browsers won't offer it at the new address, so add a new one there.
