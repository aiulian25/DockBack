package main

import _ "embed"

// recoverScript is the offline recovery tool (F35) embedded into the binary so
// the running instance can serve it and fingerprint it for the recovery kit
// ("restore without DockBack"). The SAME file is attached as a per-version GitHub
// release asset by the release checklist, so its hash matches what the app reports.
//
//go:embed scripts/dockback-recover.py
var recoverScript []byte
