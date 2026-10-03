#!/usr/bin/env python3
# dockback-recover.py — recover a DockBack (.dback) backup WITHOUT DockBack.
#
# DockBack backups are AES-256-GCM encrypted in a self-describing, chunked
# "DBACKv1" stream, documented so a backup stays recoverable even if DockBack
# itself is gone. This single, dependency-free script reimplements exactly that
# format so that, given your 64-hex encryption key and a `.dback` file, you can
# decrypt and unpack the archive on any machine with Python 3 — no DockBack, no
# Docker, no Go toolchain, no network.
#
#   python3 dockback-recover.py --key <64-hex|@keyfile> --in backup.dback \
#           [--manifest backup.dback.manifest.json] [--verify] [--out backup.tar]
#
# It prefers the `cryptography` package for speed when installed, and otherwise
# falls back to a pure-stdlib AES-256-GCM implementation so it runs with nothing
# but a stock Python 3. Decompression uses the stdlib for gzip/xz; for the
# default zstd it uses the `zstandard`/`pyzstd` module or the `zstd` CLI if
# present, and otherwise leaves the decompressed-in-a-moment command for you to
# run by hand.
#
# On-disk format (must match internal/crypto/crypto.go exactly):
#   magic[7]        = b"DBACKv1"
#   noncePrefix[4]  = random, shared by all frames
#   frame* :
#     flag[1]       = 0 (more) | 1 (final)   -- authenticated as GCM AAD
#     len[4]        = big-endian ciphertext length (ciphertext INCLUDES the tag)
#     ct[len]       = AES-256-GCM(chunk), nonce = noncePrefix || uint64_be(counter)
#   counter starts at 0 and increments per frame; the final flag is authenticated
#   so truncation is caught, and the per-frame counter catches reordering.
#
# The whole file (magic + prefix + every frame) is what the manifest's
# `cipher_sha256` is computed over, so `--verify` hashes the input file as-is.

import argparse
import base64
import hashlib
import hmac
import json
import os
import shutil
import subprocess
import sys

MAGIC = b"DBACKv1"
# App-backups (DockBack's own control-plane DB) are a distinct artifact. A v2
# (envelope) app-backup begins with this magic, then a length-prefixed plaintext
# JSON header carrying the wrapped per-backup key, then the DBACKv1 crypto stream.
# A v1 app-backup has no header and looks like any other DBACKv1 stream whose
# plaintext happens to be a raw (uncompressed) tar.
APP_MAGIC = b"DBCFGv2\n"
APP_MAX_HEADER = 1 << 16


def die(msg, code=1):
    print("error: " + msg, file=sys.stderr)
    sys.exit(code)


# --------------------------------------------------------------------------
# AES-256-GCM: a fast path via the `cryptography` package, and a correct,
# dependency-free pure-Python fallback so the tool runs on a stock Python 3.
# GCM decryption only ever uses AES *encryption* (of counter blocks), so the
# fallback implements the AES block cipher's encrypt path plus GHASH.
# --------------------------------------------------------------------------

try:
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM as _AESGCM

    _HAVE_CRYPTOGRAPHY = True
except Exception:  # pragma: no cover - environment dependent
    _HAVE_CRYPTOGRAPHY = False


def gcm_open(key, nonce, ct_and_tag, aad):
    """AES-256-GCM decrypt-and-verify. Returns plaintext, or raises on a bad tag
    (wrong key / corruption / tampering). ct_and_tag is ciphertext||tag (the Go
    layout). aad is the authenticated-but-unencrypted associated data."""
    if _HAVE_CRYPTOGRAPHY:
        # AESGCM.decrypt expects ciphertext||tag and raises InvalidTag on mismatch.
        return _AESGCM(key).decrypt(nonce, ct_and_tag, aad)
    return _pure_gcm_open(key, nonce, ct_and_tag, aad)


# ---- pure-Python AES (encrypt path only) ----

_SBOX = (
    0x63, 0x7C, 0x77, 0x7B, 0xF2, 0x6B, 0x6F, 0xC5, 0x30, 0x01, 0x67, 0x2B, 0xFE, 0xD7, 0xAB, 0x76,
    0xCA, 0x82, 0xC9, 0x7D, 0xFA, 0x59, 0x47, 0xF0, 0xAD, 0xD4, 0xA2, 0xAF, 0x9C, 0xA4, 0x72, 0xC0,
    0xB7, 0xFD, 0x93, 0x26, 0x36, 0x3F, 0xF7, 0xCC, 0x34, 0xA5, 0xE5, 0xF1, 0x71, 0xD8, 0x31, 0x15,
    0x04, 0xC7, 0x23, 0xC3, 0x18, 0x96, 0x05, 0x9A, 0x07, 0x12, 0x80, 0xE2, 0xEB, 0x27, 0xB2, 0x75,
    0x09, 0x83, 0x2C, 0x1A, 0x1B, 0x6E, 0x5A, 0xA0, 0x52, 0x3B, 0xD6, 0xB3, 0x29, 0xE3, 0x2F, 0x84,
    0x53, 0xD1, 0x00, 0xED, 0x20, 0xFC, 0xB1, 0x5B, 0x6A, 0xCB, 0xBE, 0x39, 0x4A, 0x4C, 0x58, 0xCF,
    0xD0, 0xEF, 0xAA, 0xFB, 0x43, 0x4D, 0x33, 0x85, 0x45, 0xF9, 0x02, 0x7F, 0x50, 0x3C, 0x9F, 0xA8,
    0x51, 0xA3, 0x40, 0x8F, 0x92, 0x9D, 0x38, 0xF5, 0xBC, 0xB6, 0xDA, 0x21, 0x10, 0xFF, 0xF3, 0xD2,
    0xCD, 0x0C, 0x13, 0xEC, 0x5F, 0x97, 0x44, 0x17, 0xC4, 0xA7, 0x7E, 0x3D, 0x64, 0x5D, 0x19, 0x73,
    0x60, 0x81, 0x4F, 0xDC, 0x22, 0x2A, 0x90, 0x88, 0x46, 0xEE, 0xB8, 0x14, 0xDE, 0x5E, 0x0B, 0xDB,
    0xE0, 0x32, 0x3A, 0x0A, 0x49, 0x06, 0x24, 0x5C, 0xC2, 0xD3, 0xAC, 0x62, 0x91, 0x95, 0xE4, 0x79,
    0xE7, 0xC8, 0x37, 0x6D, 0x8D, 0xD5, 0x4E, 0xA9, 0x6C, 0x56, 0xF4, 0xEA, 0x65, 0x7A, 0xAE, 0x08,
    0xBA, 0x78, 0x25, 0x2E, 0x1C, 0xA6, 0xB4, 0xC6, 0xE8, 0xDD, 0x74, 0x1F, 0x4B, 0xBD, 0x8B, 0x8A,
    0x70, 0x3E, 0xB5, 0x66, 0x48, 0x03, 0xF6, 0x0E, 0x61, 0x35, 0x57, 0xB9, 0x86, 0xC1, 0x1D, 0x9E,
    0xE1, 0xF8, 0x98, 0x11, 0x69, 0xD9, 0x8E, 0x94, 0x9B, 0x1E, 0x87, 0xE9, 0xCE, 0x55, 0x28, 0xDF,
    0x8C, 0xA1, 0x89, 0x0D, 0xBF, 0xE6, 0x42, 0x68, 0x41, 0x99, 0x2D, 0x0F, 0xB0, 0x54, 0xBB, 0x16,
)
_RCON = (0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80, 0x1B, 0x36, 0x6C, 0xD8, 0xAB, 0x4D)


def _xtime(a):
    a <<= 1
    if a & 0x100:
        a ^= 0x11B
    return a & 0xFF


def _key_expansion_256(key):
    """Return 15 round keys (each 16 bytes) for AES-256."""
    nk, nr = 8, 14
    words = [list(key[4 * i:4 * i + 4]) for i in range(nk)]
    for i in range(nk, 4 * (nr + 1)):
        temp = list(words[i - 1])
        if i % nk == 0:
            temp = temp[1:] + temp[:1]  # RotWord
            temp = [_SBOX[b] for b in temp]  # SubWord
            temp[0] ^= _RCON[i // nk - 1]
        elif i % nk == 4:
            temp = [_SBOX[b] for b in temp]
        words.append([words[i - nk][j] ^ temp[j] for j in range(4)])
    round_keys = []
    for r in range(nr + 1):
        rk = bytearray()
        for w in range(4):
            rk.extend(words[r * 4 + w])
        round_keys.append(bytes(rk))
    return round_keys


def _aes_encrypt_block(round_keys, block):
    s = [block[i] ^ round_keys[0][i] for i in range(16)]
    nr = len(round_keys) - 1
    for rnd in range(1, nr):
        # SubBytes
        s = [_SBOX[b] for b in s]
        # ShiftRows (state is column-major: byte index = col*4 + row)
        s = [
            s[0], s[5], s[10], s[15],
            s[4], s[9], s[14], s[3],
            s[8], s[13], s[2], s[7],
            s[12], s[1], s[6], s[11],
        ]
        # MixColumns
        ns = [0] * 16
        for c in range(4):
            a0, a1, a2, a3 = s[4 * c], s[4 * c + 1], s[4 * c + 2], s[4 * c + 3]
            ns[4 * c] = _xtime(a0) ^ (_xtime(a1) ^ a1) ^ a2 ^ a3
            ns[4 * c + 1] = a0 ^ _xtime(a1) ^ (_xtime(a2) ^ a2) ^ a3
            ns[4 * c + 2] = a0 ^ a1 ^ _xtime(a2) ^ (_xtime(a3) ^ a3)
            ns[4 * c + 3] = (_xtime(a0) ^ a0) ^ a1 ^ a2 ^ _xtime(a3)
        s = ns
        s = [s[i] ^ round_keys[rnd][i] for i in range(16)]
    # Final round (no MixColumns)
    s = [_SBOX[b] for b in s]
    s = [
        s[0], s[5], s[10], s[15],
        s[4], s[9], s[14], s[3],
        s[8], s[13], s[2], s[7],
        s[12], s[1], s[6], s[11],
    ]
    s = [s[i] ^ round_keys[nr][i] for i in range(16)]
    return bytes(s)


# ---- pure-Python GHASH / GCM ----

_GHASH_R = 0xE1 << 120


def _gf_mult(x, y):
    """Multiply two 128-bit field elements (big-endian ints) per GCM."""
    z = 0
    v = y
    for i in range(128):
        if (x >> (127 - i)) & 1:
            z ^= v
        if v & 1:
            v = (v >> 1) ^ _GHASH_R
        else:
            v >>= 1
    return z


def _ghash(h_int, aad, ct):
    def blocks(data):
        for i in range(0, len(data), 16):
            chunk = data[i:i + 16]
            if len(chunk) < 16:
                chunk = chunk + b"\x00" * (16 - len(chunk))
            yield int.from_bytes(chunk, "big")

    y = 0
    for b in blocks(aad):
        y = _gf_mult(y ^ b, h_int)
    for b in blocks(ct):
        y = _gf_mult(y ^ b, h_int)
    lenblock = ((len(aad) * 8) << 64) | (len(ct) * 8)
    y = _gf_mult(y ^ lenblock, h_int)
    return y.to_bytes(16, "big")


def _inc32(block):
    prefix, ctr = block[:12], int.from_bytes(block[12:], "big")
    ctr = (ctr + 1) & 0xFFFFFFFF
    return prefix + ctr.to_bytes(4, "big")


def _pure_gcm_open(key, nonce, ct_and_tag, aad):
    if aad is None:
        aad = b""
    if len(key) != 32:
        raise ValueError("key must be 32 bytes")
    if len(nonce) != 12:
        raise ValueError("nonce must be 12 bytes")
    if len(ct_and_tag) < 16:
        raise ValueError("ciphertext too short (missing tag)")
    ct, tag = ct_and_tag[:-16], ct_and_tag[-16:]
    rk = _key_expansion_256(key)
    h = _aes_encrypt_block(rk, b"\x00" * 16)
    h_int = int.from_bytes(h, "big")
    j0 = nonce + b"\x00\x00\x00\x01"

    # Verify the tag BEFORE releasing any plaintext (encrypt-then-MAC discipline).
    s = _ghash(h_int, aad, ct)
    expected = bytes(a ^ b for a, b in zip(s, _aes_encrypt_block(rk, j0)))
    if not hmac.compare_digest(expected, tag):
        raise ValueError("GCM authentication failed")

    # Counter-mode decrypt.
    out = bytearray()
    counter = _inc32(j0)
    for i in range(0, len(ct), 16):
        ks = _aes_encrypt_block(rk, counter)
        chunk = ct[i:i + 16]
        out.extend(bytes(c ^ k for c, k in zip(chunk, ks)))
        counter = _inc32(counter)
    return bytes(out)


# --------------------------------------------------------------------------
# Key + manifest loading
# --------------------------------------------------------------------------

def load_key(spec):
    """Parse --key: 64 hex chars, or @path to a file containing them."""
    if spec.startswith("@"):
        path = spec[1:]
        try:
            with open(path, "r", encoding="utf-8") as f:
                spec = f.read()
        except OSError as e:
            die("cannot read key file %s: %s" % (path, e))
    spec = spec.strip()
    # A DockBack keyfile.json is password-protected, not a raw key — reject clearly.
    if spec.startswith("{"):
        die("that looks like a password-protected keyfile.json, not a raw key; "
            "reveal the 64-hex master key in DockBack (Settings -> Encryption key) "
            "and pass that")
    try:
        key = bytes.fromhex(spec)
    except ValueError:
        die("--key must be 64 hexadecimal characters (a 32-byte key)")
    if len(key) != 32:
        die("--key must be 64 hexadecimal characters (got %d bytes)" % len(key))
    return key


def load_manifest(path, key):
    """Read a manifest sidecar (plain .manifest.json, or an AES-GCM sealed
    .manifest.json.enc which is unsealed with the master key: layout nonce||ct)."""
    with open(path, "rb") as f:
        raw = f.read()
    if path.endswith(".enc"):
        if len(raw) < 12 + 16:
            die("sealed manifest %s is too short" % path)
        try:
            plain = gcm_open(key, raw[:12], raw[12:], None)
        except Exception:
            die("cannot decrypt sealed manifest — wrong key, or the file is corrupt")
        raw = plain
    try:
        return json.loads(raw)
    except ValueError as e:
        die("manifest %s is not valid JSON: %s" % (path, e))


def find_manifest(in_path):
    """Locate the manifest sidecar next to the archive if --manifest wasn't given.
    DockBack writes it as '<archive>.manifest.json' (or '.enc' when sealed)."""
    for suffix in (".manifest.json", ".manifest.json.enc"):
        cand = in_path + suffix
        if os.path.exists(cand):
            return cand
    return None


# --------------------------------------------------------------------------
# Write-only backups (F86): the archive key is sealed to an X25519 PUBLIC key
# whose private half lives offline, so DockBack itself cannot read the archive.
# Recovery therefore needs --private-key rather than (or as well as) --key.
#
# The envelope mirrors internal/crypto/writeonly.go exactly:
#
#   wrapped = base64( ephPub(32) || nonce(12) || AES-256-GCM(dek)+tag )
#   kek     = HKDF-SHA256(ikm = X25519(priv, ephPub),
#                         salt = ephPub || recipientPub,
#                         info = "dockback-write-only-v1", len = 32)
#
# X25519 is implemented here in pure Python (RFC 7748 reference ladder) so this
# script keeps its one hard promise: it depends on nothing but the standard
# library. A recovery tool that needs `pip install` is a recovery tool that
# fails on the day you need it.
# --------------------------------------------------------------------------

_P25519 = 2 ** 255 - 19
_A24 = 121665
WRITE_ONLY_INFO = b"dockback-write-only-v1"


def _cswap(swap, a, b):
    dummy = swap * ((a - b) % _P25519)
    return (a - dummy) % _P25519, (b + dummy) % _P25519


def x25519(scalar, u_bytes):
    """RFC 7748 X25519 scalar multiplication. Returns 32 little-endian bytes."""
    k = bytearray(scalar)
    k[0] &= 248
    k[31] &= 127
    k[31] |= 64
    k = int.from_bytes(bytes(k), "little")
    x1 = int.from_bytes(u_bytes, "little") & ((1 << 255) - 1)
    x2, z2, x3, z3, swap = 1, 0, x1, 1, 0
    for t in range(254, -1, -1):
        kt = (k >> t) & 1
        swap ^= kt
        x2, x3 = _cswap(swap, x2, x3)
        z2, z3 = _cswap(swap, z2, z3)
        swap = kt
        a = (x2 + z2) % _P25519
        aa = (a * a) % _P25519
        b = (x2 - z2) % _P25519
        bb = (b * b) % _P25519
        e = (aa - bb) % _P25519
        c = (x3 + z3) % _P25519
        d = (x3 - z3) % _P25519
        da = (d * a) % _P25519
        cb = (c * b) % _P25519
        x3 = pow((da + cb) % _P25519, 2, _P25519)
        z3 = (x1 * pow((da - cb) % _P25519, 2, _P25519)) % _P25519
        x2 = (aa * bb) % _P25519
        z2 = (e * ((aa + _A24 * e) % _P25519)) % _P25519
    x2, x3 = _cswap(swap, x2, x3)
    z2, z3 = _cswap(swap, z2, z3)
    return ((x2 * pow(z2, _P25519 - 2, _P25519)) % _P25519).to_bytes(32, "little")


def hkdf_sha256(ikm, salt, info, length):
    """RFC 5869 HKDF-SHA256 — matches Go's crypto/hkdf Key()."""
    prk = hmac.new(salt, ikm, hashlib.sha256).digest()
    okm, t, i = b"", b"", 1
    while len(okm) < length:
        t = hmac.new(prk, t + info + bytes([i]), hashlib.sha256).digest()
        okm += t
        i += 1
    return okm[:length]


def load_private_key(spec):
    """Parse --private-key: base64 X25519 key, or @path to a file holding it."""
    if spec.startswith("@"):
        path = spec[1:]
        try:
            with open(path, "r", encoding="utf-8") as f:
                spec = f.read()
        except OSError as e:
            die("cannot read private key file %s: %s" % (path, e))
    spec = spec.strip()
    try:
        raw = base64.b64decode(spec, validate=True)
    except Exception:
        die("--private-key must be the base64 key exactly as DockBack issued it")
    if len(raw) != 32:
        die("--private-key must decode to 32 bytes (got %d)" % len(raw))
    return raw


def unwrap_key_private(wrapped_b64, priv):
    """Recover the archive DEK from a write-only envelope using the offline key."""
    try:
        blob = base64.b64decode(wrapped_b64, validate=True)
    except Exception:
        die("manifest wrapped_key_pub is not valid base64")
    if len(blob) < 32 + 12 + 16:
        die("manifest wrapped_key_pub is too short")
    eph_pub, sealed = blob[:32], blob[32:]
    shared = x25519(priv, eph_pub)
    if shared == b"\x00" * 32:
        die("invalid write-only envelope (degenerate shared secret)")
    recipient_pub = x25519(priv, (9).to_bytes(32, "little"))
    kek = hkdf_sha256(shared, eph_pub + recipient_pub, WRITE_ONLY_INFO, 32)
    try:
        dek = gcm_open(kek, sealed[:12], sealed[12:], None)
    except Exception:
        die("the private key does not match this backup — check you are holding the "
            "right recovery sheet (the manifest's backup_pub_fp names the keypair)")
    if len(dek) != 32:
        die("unwrapped archive key has the wrong length")
    return dek


def resolve_archive_key(master, manifest, private_key=None):
    """Return the key that actually decrypts the archive (envelope DEK unwrapped
    with the master key, or the master key itself for legacy backups), mirroring
    internal/backup/engine.go archiveKey / internal/crypto UnwrapKey.

    A WRITE-ONLY backup is checked first: its DEK is sealed to an offline public
    key, so the master key is irrelevant and must never be silently tried."""
    if manifest and manifest.get("wrapped_key_pub"):
        if not private_key:
            die("this backup is write-only encrypted — pass --private-key with the "
                "offline key from its recovery sheet (the master key cannot open it)")
        return unwrap_key_private(manifest["wrapped_key_pub"], private_key), True
    if manifest and manifest.get("wrapped_key"):
        if master is None:
            die("this backup's key is wrapped with the master key — pass --key")
        try:
            blob = base64.b64decode(manifest["wrapped_key"], validate=True)
        except Exception:
            die("manifest wrapped_key is not valid base64")
        if len(blob) < 12 + 16:
            die("manifest wrapped_key is too short")
        try:
            dek = gcm_open(master, blob[:12], blob[12:], None)
        except Exception:
            die("cannot unwrap the archive key — this backup was encrypted with a "
                "different master key than the one you provided")
        if len(dek) != 32:
            die("unwrapped archive key has the wrong length")
        return dek, True
    if master is None:
        die("this backup is encrypted directly with the master key — pass --key")
    return master, False  # legacy: archive encrypted directly with the master key


# --------------------------------------------------------------------------
# Verify, summary, decrypt, decompress
# --------------------------------------------------------------------------

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def print_summary(manifest, enveloped):
    if not manifest:
        print("Manifest: (none supplied — pass --manifest for a full summary)")
        return
    fmt = manifest.get("format", {}) or {}
    print("Backup manifest")
    print("  target      : %s" % manifest.get("target_name", "?"))
    if manifest.get("stack"):
        print("  stack       : %s" % manifest["stack"])
    print("  image       : %s" % manifest.get("image", "?"))
    if manifest.get("image_digest"):
        print("  image digest: %s" % manifest["image_digest"])
    print("  created     : %s" % manifest.get("created_at", "?"))
    print("  node        : %s" % manifest.get("node_name", "?"))
    print("  encryption  : %s%s" % (
        fmt.get("encryption", "AES-256-GCM (DBACKv1 chunked)"),
        " [envelope: per-backup key]" if enveloped else " [direct master key]"))
    print("  compression : %s (%s)" % (
        fmt.get("compression", "?"), fmt.get("algorithm") or "zstd"))
    vols = manifest.get("volumes") or []
    if vols:
        print("  volumes     :")
        for v in vols:
            print("    - %s%s" % (v.get("destination", "?"),
                                  (" (%s)" % v["name"]) if v.get("name") else ""))
    dbs = manifest.get("databases") or []
    if dbs:
        print("  databases   :")
        for d in dbs:
            ver = (" %s" % d["version"]) if d.get("version") else ""
            print("    - %s (%s%s) -> %s" % (
                d.get("service", "?"), d.get("engine", "?"), ver, d.get("path", "?")))
    skipped = manifest.get("skipped_mounts") or []
    if skipped:
        print("  PARTIAL     : %d mount(s) were NOT captured:" % len(skipped))
        for m in skipped:
            print("    - %s (%s)" % (m.get("destination", "?"), m.get("reason", "?")))
    # Incremental volume backup (F61): this archive holds only CHANGED files; a
    # correct restore needs the whole parent chain applied full -> deltas.
    if manifest.get("incremental"):
        deleted = manifest.get("deleted") or []
        print("  INCREMENTAL : delta backup (changed files only)")
        print("    parent    : %s" % (manifest.get("parent") or "?"))
        print("    depth     : %d since the full baseline" % manifest.get("chain_depth", 0))
        if deleted:
            print("    deletes   : %d path(s) removed since the parent" % len(deleted))
        print("    NOTE      : this archive alone is NOT a complete restore — supply the")
        print("                whole chain's .dback files to --apply-chain to rebuild it.")
    elif manifest.get("vol_index"):
        print("  INCREMENTAL : full baseline (a later delta chain may build on this)")


def decrypt_stream(f, out, key):
    """Decrypt the DBACKv1 stream read from file object f into out. f must be
    positioned at the DBACKv1 magic (offset 0 for a container/v1 archive, or right
    after the plaintext header for a v2 app-backup). Verifies every frame's GCM tag;
    the first frame authenticates before any output, so a wrong key fails cleanly
    with no output."""
    header = f.read(len(MAGIC) + 4)
    if len(header) < len(MAGIC) + 4:
        die("truncated archive: missing header")
    if header[:len(MAGIC)] != MAGIC:
        die("bad magic: not a DockBack archive")
    prefix = header[len(MAGIC):]
    counter = 0
    while True:
        fhdr = f.read(5)
        if len(fhdr) < 5:
            die("truncated archive: incomplete frame header (frame %d)" % counter)
        flag = fhdr[0:1]
        ln = int.from_bytes(fhdr[1:5], "big")
        ct = f.read(ln)
        if len(ct) < ln:
            die("truncated archive: incomplete frame body (frame %d)" % counter)
        nonce = prefix + counter.to_bytes(8, "big")
        try:
            pt = gcm_open(key, nonce, ct, flag)
        except Exception:
            die("frame %d failed authentication — wrong key, or the archive is "
                "corrupt or tampered with (no output was written)" % counter)
        out.write(pt)
        counter += 1
        if flag == b"\x01":
            break


def decrypt_archive(in_path, key, out_compressed):
    """Decrypt a whole container/v1 archive file to out_compressed."""
    with open(in_path, "rb") as f, open(out_compressed, "wb") as out:
        decrypt_stream(f, out, key)


def sniff_compression(path):
    """Identify the compression of a decrypted stream by its leading magic, so we
    decompress container archives (zstd/gzip/xz) but leave an already-plain tar
    (an app-backup) untouched. Returns 'zstd'|'gzip'|'xz', or None for a plain tar."""
    with open(path, "rb") as f:
        head = f.read(6)
    if head[:4] == b"\x28\xb5\x2f\xfd":
        return "zstd"
    if head[:2] == b"\x1f\x8b":
        return "gzip"
    if head[:6] == b"\xfd7zXZ\x00":
        return "xz"
    return None


def decompress(algo, src, dst):
    """Decompress src (a compressed tar) to dst (a plain tar). Returns True on
    success; False means the caller should keep the compressed file and print the
    manual command (only happens for zstd with no module and no `zstd` binary)."""
    algo = (algo or "zstd").lower()
    if algo == "gzip":
        import gzip
        with gzip.open(src, "rb") as r, open(dst, "wb") as w:
            shutil.copyfileobj(r, w, length=1 << 20)
        return True
    if algo == "xz":
        import lzma
        with lzma.open(src, "rb") as r, open(dst, "wb") as w:
            shutil.copyfileobj(r, w, length=1 << 20)
        return True
    # zstd (the default). Try a Python module, then the `zstd` CLI.
    for modname in ("zstandard", "pyzstd"):
        try:
            mod = __import__(modname)
        except Exception:
            continue
        with open(src, "rb") as r, open(dst, "wb") as w:
            if modname == "zstandard":
                mod.ZstdDecompressor().copy_stream(r, w)
            else:
                w.write(mod.decompress(r.read()))
        return True
    if shutil.which("zstd"):
        subprocess.run(["zstd", "-d", "-q", "-f", "-o", dst, src], check=True)
        return True
    return False


def is_app_backup(in_path):
    """Report whether in_path is a v2 (envelope) app-backup, by its header magic."""
    with open(in_path, "rb") as f:
        return f.read(len(APP_MAGIC)) == APP_MAGIC


def recover_app_backup(args, master):
    """Recover a v2 (envelope) app-backup: read the plaintext header, unwrap the DEK
    with the master key, and decrypt the DBACKv1 body to the output tar. App-backups
    are NOT compressed, so the decrypted stream is already a plain tar."""
    if not _HAVE_CRYPTOGRAPHY:
        print("note: using the pure-Python AES-GCM fallback (no 'cryptography' "
              "package) — correct but slow; install 'cryptography' to speed it up.",
              file=sys.stderr)
    out_path = args.out or (args.infile[:-len(".dback")] + ".tar"
                            if args.infile.endswith(".dback") else args.infile + ".tar")
    tmp_out = out_path + ".part"
    try:
        with open(args.infile, "rb") as f:
            f.read(len(APP_MAGIC))  # consume the magic
            lb = f.read(4)
            if len(lb) < 4:
                die("truncated app-backup: missing header length")
            hlen = int.from_bytes(lb, "big")
            if hlen == 0 or hlen > APP_MAX_HEADER:
                die("invalid app-backup header length %d" % hlen)
            hb = f.read(hlen)
            if len(hb) < hlen:
                die("truncated app-backup: incomplete header")
            hdr = json.loads(hb)
            wrapped = hdr.get("wrapped_key")
            if not wrapped:
                die("app-backup header has no wrapped_key")
            try:
                blob = base64.b64decode(wrapped, validate=True)
            except Exception:
                die("app-backup wrapped_key is not valid base64")
            if len(blob) < 12 + 16:
                die("app-backup wrapped_key is too short")
            try:
                dek = gcm_open(master, blob[:12], blob[12:], None)
            except Exception:
                die("cannot unwrap app-backup key — wrong master encryption key")
            if len(dek) != 32:
                die("unwrapped app-backup key has the wrong length")

            print("DockBack application backup (its own control-plane database)")
            print("  format      : %s" % hdr.get("format", "?"))
            print("  version     : %s (envelope: per-backup key)" % hdr.get("version", "?"))
            if hdr.get("app_version"):
                print("  app version : %s" % hdr["app_version"])
            if hdr.get("key_fingerprint"):
                print("  key fp      : %s" % hdr["key_fingerprint"])

            print("Decrypting…")
            # f is positioned at the DBACKv1 body; decrypt straight to the plain tar.
            with open(tmp_out, "wb") as out:
                decrypt_stream(f, out, dek)
        os.replace(tmp_out, out_path)
        print("\nRecovered: %s" % out_path)
        print("It contains DockBack's own database:")
        print("  tar -tf %s   # manifest.json, dockback.db" % out_path)
        print("  tar -xf %s" % out_path)
    except BaseException:
        if os.path.exists(tmp_out):
            os.remove(tmp_out)
        raise


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------

def _decrypt_one_to_tar(dback_path, manifest, master, out_tar, private_key=None):
    """Decrypt+decompress a single .dback to a plain tar at out_tar."""
    arch_key, _ = resolve_archive_key(master, manifest, private_key)
    tmp_c = out_tar + ".compressed.part"
    try:
        decrypt_archive(dback_path, arch_key, tmp_c)
        sniffed = sniff_compression(tmp_c)
        if sniffed is None:
            os.replace(tmp_c, out_tar)
            return
        if not decompress(sniffed, tmp_c, out_tar):
            die("no zstd available to decompress %s — install 'cryptography'/'zstandard' "
                "or the zstd CLI" % dback_path)
        os.remove(tmp_c)
    finally:
        if os.path.exists(tmp_c):
            os.remove(tmp_c)


def apply_chain(args, master, private_key=None):
    """Resolve an incremental backup's parent chain from a directory of .dback files
    and emit ordered plain tars plus an APPLY_ORDER.txt describing how to rebuild the
    volume state (F61): extract the full baseline, then each delta OVER it, removing
    the paths each delta deleted. Fails closed if any link is missing or its recorded
    parent checksum doesn't match (the same cryptographic pin the app enforces)."""
    chain_dir = args.apply_chain
    if not os.path.isdir(chain_dir):
        die("--apply-chain must be a directory containing the chain's .dback files: %s" % chain_dir)

    # Index every .dback in the directory by its backup id, with its manifest.
    by_id = {}       # backup_id -> {"dback":path, "man":manifest}
    for name in sorted(os.listdir(chain_dir)):
        if not name.endswith(".dback"):
            continue
        p = os.path.join(chain_dir, name)
        mpath = find_manifest(p)
        if not mpath:
            continue
        man = load_manifest(mpath, master)
        bid = man.get("backup_id")
        if bid:
            by_id[bid] = {"dback": p, "man": man}

    # Start from the target archive's manifest and walk parents to the full baseline.
    target_mpath = args.manifest or find_manifest(args.infile)
    if not target_mpath:
        die("cannot find the manifest for --in; pass --manifest")
    target_man = load_manifest(target_mpath, master)
    chain = []
    seen = set()
    cur = target_man.get("backup_id")
    # Make sure the target itself is indexed (it may live outside chain_dir).
    if cur and cur not in by_id:
        by_id[cur] = {"dback": args.infile, "man": target_man}
    while cur:
        if cur in seen:
            die("backup chain has a cycle at %s" % cur)
        seen.add(cur)
        node = by_id.get(cur)
        if not node:
            die("backup chain is broken: %s is missing from %s" % (cur, chain_dir))
        chain.append(node)
        man = node["man"]
        parent = man.get("parent")
        if not parent:
            break
        pnode = by_id.get(parent)
        if not pnode:
            die("backup chain is broken: parent %s of %s is missing from %s" % (parent, cur, chain_dir))
        pin = man.get("parent_cipher_sha256")
        actual = pnode["man"].get("cipher_sha256")
        if pin and actual and pin != actual:
            die("backup chain integrity check failed: parent %s does not match the pin in %s" % (parent, cur))
        cur = parent
    chain.reverse()  # oldest (full) -> newest
    if not chain or chain[0]["man"].get("incremental"):
        die("backup chain has no full baseline")

    out_dir = args.out or (args.infile + ".chain")
    os.makedirs(out_dir, exist_ok=True)
    order_lines = [
        "DockBack incremental restore — apply these tars IN THIS ORDER into one directory.",
        "Each delta is extracted OVER the previous state; then remove the listed deleted paths.",
        "",
    ]
    for i, node in enumerate(chain):
        man = node["man"]
        kind = "full baseline" if not man.get("incremental") else "delta"
        tar_name = "%02d_%s.tar" % (i, (man.get("backup_id") or "gen")[:12])
        out_tar = os.path.join(out_dir, tar_name)
        print("Decrypting %s (%s)…" % (man.get("backup_id", "?")[:12], kind))
        _decrypt_one_to_tar(node["dback"], man, master, out_tar, private_key)
        order_lines.append("%d) %s  [%s]" % (i + 1, tar_name, kind))
        order_lines.append("     tar -xf %s -C RESTORE_DIR" % tar_name)
        deleted = man.get("deleted") or []
        if deleted:
            del_name = "%02d_%s.deleted.txt" % (i, (man.get("backup_id") or "gen")[:12])
            with open(os.path.join(out_dir, del_name), "w") as df:
                df.write("\n".join(deleted) + "\n")
            order_lines.append("     # then remove %d path(s) listed in %s:" % (len(deleted), del_name))
            order_lines.append("     (cd RESTORE_DIR && xargs -a ../%s rm -f)" % del_name)
        order_lines.append("")
    with open(os.path.join(out_dir, "APPLY_ORDER.txt"), "w") as of:
        of.write("\n".join(order_lines))
    print("\nRecovered %d generation(s) into: %s" % (len(chain), out_dir))
    print("Follow: %s" % os.path.join(out_dir, "APPLY_ORDER.txt"))


def main():
    ap = argparse.ArgumentParser(
        description="Recover a DockBack (.dback) backup without DockBack.",
        epilog="Example: dockback-recover.py --key @key.txt --in b.dback --verify --out b.tar")
    ap.add_argument("--key", metavar="HEX|@FILE",
                    help="64-hex master key, or @path to a file containing it. "
                         "Required for normal backups; a write-only backup with a "
                         "plaintext manifest needs only --private-key")
    ap.add_argument("--private-key", dest="private_key", metavar="B64|@FILE",
                    help="offline X25519 private key (base64, or @path) for a "
                         "WRITE-ONLY backup — the only thing that can decrypt it")
    ap.add_argument("--in", dest="infile", required=True, metavar="FILE",
                    help="the .dback archive to recover")
    ap.add_argument("--out", metavar="FILE",
                    help="output tar path (default: <input>.tar)")
    ap.add_argument("--manifest", metavar="FILE",
                    help="the .manifest.json sidecar (auto-detected next to --in if omitted)")
    ap.add_argument("--verify", action="store_true",
                    help="check the archive's SHA-256 against the manifest before decrypting")
    ap.add_argument("--apply-chain", metavar="DIR",
                    help="for an incremental backup: resolve the parent chain from the "
                         ".dback files in DIR and emit ordered tars + APPLY_ORDER.txt "
                         "(--out is the output directory)")
    args = ap.parse_args()

    if not os.path.exists(args.infile):
        die("input archive not found: %s" % args.infile)

    key = load_key(args.key) if args.key else None
    priv = load_private_key(args.private_key) if args.private_key else None

    # Incremental chain reconstruction (F61) — emit ordered tars for the whole chain.
    if args.apply_chain:
        if key is None:
            die("--apply-chain needs --key (chain manifests and parents are read "
                "with the master key)")
        apply_chain(args, key, priv)
        return

    # An app-backup (DockBack's own DB) is a distinct artifact with its own header.
    if is_app_backup(args.infile):
        if args.verify:
            print("note: --verify applies to container backups (which carry a "
                  "manifest checksum); an app-backup is integrity-checked by its "
                  "GCM tags instead.", file=sys.stderr)
        if key is None:
            die("an app-backup is sealed with the master key — pass --key")
        recover_app_backup(args, key)
        return

    manifest_path = args.manifest or find_manifest(args.infile)
    if manifest_path and manifest_path.endswith(".enc") and key is None:
        die("this backup's manifest sidecar is sealed with the master key — pass "
            "--key as well as --private-key")
    manifest = load_manifest(manifest_path, key) if manifest_path else None
    if args.manifest and not os.path.exists(args.manifest):
        die("manifest not found: %s" % args.manifest)

    if not _HAVE_CRYPTOGRAPHY:
        print("note: using the pure-Python AES-GCM fallback (no 'cryptography' package). "
              "This is correct but slow for large archives — install 'cryptography' "
              "(pip install cryptography) to speed it up.", file=sys.stderr)

    # --verify: the manifest's cipher_sha256 is over the WHOLE input file.
    if args.verify:
        if not manifest:
            die("--verify needs the manifest (pass --manifest or place the "
                "'<archive>.manifest.json' sidecar next to the archive)")
        want = manifest.get("cipher_sha256", "")
        if not want:
            die("manifest has no cipher_sha256 to verify against")
        print("Verifying archive checksum (SHA-256 of the ciphertext)…")
        got = sha256_file(args.infile)
        if not hmac.compare_digest(got, want):
            die("checksum MISMATCH — the archive does not match its manifest.\n"
                "  expected %s\n  got      %s" % (want, got))
        print("  OK: %s" % got)

    arch_key, enveloped = resolve_archive_key(key, manifest, priv)
    print_summary(manifest, enveloped)

    out_path = args.out or (args.infile + ".tar" if not args.infile.endswith(".dback")
                            else args.infile[:-len(".dback")] + ".tar")
    algo = ((manifest or {}).get("format", {}) or {}).get("algorithm", "") if manifest else ""

    # Decrypt to a temp compressed file, then decompress to a temp output, then
    # atomically move into place — so a failure never leaves partial/garbage output.
    tmp_compressed = out_path + ".compressed.part"
    tmp_out = out_path + ".part"
    try:
        print("Decrypting archive…")
        decrypt_archive(args.infile, arch_key, tmp_compressed)
        # Choose the decompressor from the decrypted stream's own magic (robust, and
        # it recognises an already-plain tar — e.g. a legacy app-backup — as "none").
        sniffed = sniff_compression(tmp_compressed)
        if sniffed is None:
            os.replace(tmp_compressed, out_path)
            if os.path.exists(tmp_out):
                os.remove(tmp_out)
            print("\nRecovered: %s" % out_path)
            print("List contents:  tar -tf %s" % out_path)
            print("Extract all:    tar -xf %s" % out_path)
            return
        algo = sniffed
        print("Decompressing (%s)…" % algo)
        if decompress(algo, tmp_compressed, tmp_out):
            os.replace(tmp_out, out_path)
            os.remove(tmp_compressed)
            print("\nRecovered: %s" % out_path)
            print("List contents:  tar -tf %s" % out_path)
            print("Extract all:    tar -xf %s" % out_path)
        else:
            # No zstd available — keep the compressed tar and tell the user the
            # one command that finishes the job.
            compressed_path = out_path + ".zst"
            os.replace(tmp_compressed, compressed_path)
            if os.path.exists(tmp_out):
                os.remove(tmp_out)
            print("\nDecrypted, but this machine has no zstd to decompress it.")
            print("Recovered (compressed): %s" % compressed_path)
            print("Finish with either:")
            print("  zstd -d %s -o %s" % (compressed_path, out_path))
            print("  tar --zstd -xf %s" % compressed_path)
    except SystemExit:
        for p in (tmp_compressed, tmp_out):
            if os.path.exists(p):
                os.remove(p)
        raise
    except BaseException:
        for p in (tmp_compressed, tmp_out):
            if os.path.exists(p):
                os.remove(p)
        raise


if __name__ == "__main__":
    main()
