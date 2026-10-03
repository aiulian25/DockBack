import { beforeEach, expect, it } from "vitest";
import { readStackRestoreSeed, writeStackRestoreSeed } from "./stackRestoreSeed";

// The seed carries a caller's restore choices to the stack-restore page via
// one-shot sessionStorage. Two guarantees matter (F224): it is CONSUMED on read
// (so it can't be replayed) and it is REFUSED for a different stack (so one
// stack's selections can never be applied to another's restore).

beforeEach(() => sessionStorage.clear());

it("round-trips a seed for the matching stack, then consumes it", () => {
  writeStackRestoreSeed({ nodeID: "n1", project: "blog", group: "g1", recreate: true });

  const got = readStackRestoreSeed("n1", "blog");
  expect(got).toMatchObject({ nodeID: "n1", project: "blog", group: "g1", recreate: true });

  // Second read finds nothing — the seed is one-shot.
  expect(readStackRestoreSeed("n1", "blog")).toBeNull();
});

it("refuses a seed left for a different stack, and clears it", () => {
  writeStackRestoreSeed({ nodeID: "n1", project: "blog" });

  // Wrong stack → null…
  expect(readStackRestoreSeed("n1", "other")).toBeNull();
  // …and it was removed, so even the right stack now gets nothing (never applied
  // to a later restore of a different stack).
  expect(readStackRestoreSeed("n1", "blog")).toBeNull();
});

it("returns null when nothing was written", () => {
  expect(readStackRestoreSeed("n1", "blog")).toBeNull();
});

it("returns null on a corrupt seed rather than throwing", () => {
  sessionStorage.setItem("dback.stack_restore_seed", "{not valid json");
  expect(readStackRestoreSeed("n1", "blog")).toBeNull();
});
