package backup

import (
	"strconv"
	"strings"
)

// RestoreCompatibility checks whether a backup's database dumps can be safely
// restored into the target container's image, returning human-readable warnings
// and whether the mismatch is severe enough to BLOCK the restore unless the user
// explicitly overrides (F10).
//
// The classic break it guards against is the Immich pgvecto.rs -> VectorChord
// upgrade: a logical dump that needs the `vectors` extension (pgvecto.rs)
// restored into a VectorChord image (or plain Postgres) fails deep inside psql
// on import or, worse, appears to succeed and corrupts vector search. It also
// catches an outright engine-family mismatch (e.g. a Postgres dump into a MySQL
// image). Non-database and extension-free backups never block.
func RestoreCompatibility(man *Manifest, targetImage, targetVersion string, targetExts []string) (warnings []string, blocking bool) {
	if man == nil {
		return nil, false
	}
	targetEngine := detectDBEngine(targetImage, nil)
	// F41: prefer the target's MEASURED extensions (a live probe of the running
	// container) over a guess from the image name — a privately-tagged image can
	// hide or falsely imply a vector extension, causing this gate to block a valid
	// restore or pass a broken one. targetExts != nil means we probed successfully
	// (possibly finding none); nil means we couldn't probe (stopped/unreachable),
	// so we fall back to the image-name guess and SAY SO in the warning.
	probedExts := targetExts != nil
	targetVec := imageVectorFamily(targetImage)
	if probedExts {
		targetVec = dumpVectorFamily(targetExts)
	}
	targetMajor, targetMajorOK := parseMajor(targetVersion)
	for _, db := range man.Databases {
		// Engine-family mismatch: both sides confidently detected and different
		// (e.g. a postgres dump restored into a mysql/mongo/redis image). The dump
		// can't import into a different engine, so block.
		if db.Engine != "" && targetEngine != "" && db.Engine != targetEngine {
			warnings = append(warnings,
				"This backup's database \""+db.Service+"\" is a "+db.Engine+" dump, but the target image ("+targetImage+") is a "+targetEngine+" database. Restoring it there will fail.")
			blocking = true
			continue
		}
		// Engine version DOWNGRADE (F36): a newer dump can't be restored into an
		// older engine — it fails deep in the import, AFTER the data dir has been
		// re-initialized. Block that; an upgrade only warns; an equal major (or an
		// unreadable version on either side) passes silently.
		if dumpMajor, dumpOK := parseMajor(db.Version); dumpOK && targetMajorOK && dumpMajor != targetMajor {
			eng := db.Engine
			if eng == "" {
				eng = targetEngine
			}
			if dumpMajor > targetMajor {
				warnings = append(warnings,
					"This backup's database \""+db.Service+"\" is a "+eng+" "+strconv.Itoa(dumpMajor)+" dump, but the target image ("+targetImage+") runs "+eng+" "+strconv.Itoa(targetMajor)+". A newer dump cannot be restored into an older engine — it fails partway through the import, after the data directory has been re-initialized. Restore into a "+eng+" "+strconv.Itoa(dumpMajor)+"+ image, or upgrade the target first.")
				blocking = true
				continue
			}
			warnings = append(warnings,
				"This backup's database \""+db.Service+"\" is a "+eng+" "+strconv.Itoa(dumpMajor)+" dump being restored into "+eng+" "+strconv.Itoa(targetMajor)+" (an upgrade). This normally works, but review the engine's upgrade notes if the import reports errors.")
		}
		// Vector-extension family mismatch (the pgvecto.rs / VectorChord / pgvector
		// break). Only Postgres carries these; a dump with no vector extension never
		// blocks.
		need := dumpVectorFamily(db.Extensions)
		if need == "" {
			continue
		}
		// F131: the SAME family can still be an incompatible VERSION. The family
		// check above catches the pgvecto.rs -> VectorChord migration; this catches
		// the quieter case of VectorChord 0.4.2 -> 0.5.0, where the index format
		// moved underneath a dump that restores without complaint and leaves
		// vector search returning wrong results.
		//
		// Only MAJOR or MINOR differences block. A patch bump is a fix release and
		// blocking on it would refuse valid restores far more often than it caught
		// a real problem — the same fail-open reasoning as everywhere else here.
		if targetVec == need && probedExts {
			if w, blocks := vectorVersionVerdict(db.Service, targetImage, need, db.Extensions, targetExts); w != "" {
				warnings = append(warnings, w)
				blocking = blocking || blocks
				continue
			}
		}
		if targetVec != need {
			targetDesc := targetVec
			if targetDesc == "" {
				targetDesc = "plain Postgres (no vector extension)"
			}
			msg := "This backup's database \"" + db.Service + "\" uses the " + need + " vector extension, but the target image (" + targetImage + ") provides " + targetDesc + ". Restoring it there can fail on import or silently corrupt vector search — reinstall/match the extension on the target, or restore into a matching image."
			if !probedExts {
				msg += " (the target's extensions could not be read — this was inferred from the image name)"
			}
			warnings = append(warnings, msg)
			blocking = true
		}
	}
	return warnings, blocking
}

// vectorVersionVerdict compares the VERSION of the vector extension a dump needs
// against the version the target actually provides (F131).
//
// Returns ("", false) when there is nothing to say — either version unreadable,
// or they match. A patch-level difference warns; a major or minor one blocks,
// because that is where these extensions change their on-disk index format and a
// dump restored across it produces a database that looks fine and searches
// wrongly.
func vectorVersionVerdict(service, targetImage, family string, dumpExts, targetExts []string) (string, bool) {
	name := vectorExtName(family)
	dumpVer := extVersion(dumpExts, name)
	targetVer := extVersion(targetExts, name)
	if dumpVer == "" || targetVer == "" || dumpVer == targetVer {
		return "", false
	}
	if sameMajorMinor(dumpVer, targetVer) {
		return "This backup's database \"" + service + "\" was dumped with " + family + " " + dumpVer +
			" and the target image (" + targetImage + ") provides " + targetVer +
			". That is a patch-level difference and normally restores cleanly — check the extension's release notes if vector search behaves oddly afterwards.", false
	}
	return "This backup's database \"" + service + "\" was dumped with " + family + " " + dumpVer +
		", but the target image (" + targetImage + ") provides " + family + " " + targetVer +
		". These versions use different vector index formats: the dump would restore without any error and leave search returning wrong results. Restore into an image providing " + family + " " + dumpVer +
		", or follow the extension's documented migration on the target first.", true
}

// vectorExtName maps a family label back to the extension name pg_extension
// reports, so a version can be looked up by the same key it was recorded under.
func vectorExtName(family string) string {
	switch family {
	case "pgvecto.rs":
		return "vectors"
	case "VectorChord":
		return "vchord"
	case "pgvector":
		return "vector"
	}
	return ""
}

// extVersion pulls one extension's version out of a recorded "name version"
// list. Empty when absent or unnamed — the caller then says nothing rather than
// comparing against a guess.
func extVersion(exts []string, name string) string {
	if name == "" {
		return ""
	}
	for _, e := range exts {
		n, v, ok := strings.Cut(strings.TrimSpace(e), " ")
		if ok && strings.EqualFold(strings.TrimSpace(n), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// sameMajorMinor reports whether two dotted versions agree on their first two
// components — the granularity at which these extensions change their index
// format. An unreadable version compares as "not the same", so the caller takes
// the cautious branch.
func sameMajorMinor(a, b string) bool {
	av, aok := versionParts(a)
	bv, bok := versionParts(b)
	if !aok || !bok {
		return false
	}
	at := func(v []int, i int) int {
		if i < len(v) {
			return v[i]
		}
		return 0
	}
	return at(av, 0) == at(bv, 0) && at(av, 1) == at(bv, 1)
}

// dumpVectorFamily returns the vector-search extension family a dump requires,
// from its recorded "name version" extension list (most specific first). Empty
// when the dump uses no vector extension. Names map to their providers:
//
//	vectors -> pgvecto.rs   vchord -> VectorChord   vector -> pgvector
func dumpVectorFamily(extensions []string) string {
	has := func(name string) bool {
		for _, e := range extensions {
			n := e // entries are "name version"; match the leading name token.
			if i := strings.IndexByte(n, ' '); i >= 0 {
				n = n[:i]
			}
			if strings.EqualFold(strings.TrimSpace(n), name) {
				return true
			}
		}
		return false
	}
	switch {
	case has("vectors"):
		return "pgvecto.rs"
	case has("vchord"):
		return "VectorChord"
	case has("vector"):
		return "pgvector"
	}
	return ""
}

// imageVectorFamily returns the vector-search family an image provides, derived
// from its name (mirrors the Postgres-family recognition in detectDBEngine).
// Returns the SAME family labels as dumpVectorFamily so the two compare directly.
func imageVectorFamily(image string) string {
	img := strings.ToLower(image)
	switch {
	case strings.Contains(img, "vchord") || strings.Contains(img, "vectorchord"):
		return "VectorChord"
	// "pgvector" must be checked BEFORE "pgvecto": the string "pgvector" contains
	// "pgvecto" as a prefix, so the more specific match has to win.
	case strings.Contains(img, "pgvector"):
		return "pgvector"
	case strings.Contains(img, "pgvecto"):
		return "pgvecto.rs"
	}
	return ""
}
