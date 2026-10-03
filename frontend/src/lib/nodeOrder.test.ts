import { expect, it } from "vitest";
import { reorderWithFilter } from "./nodeOrder";

// Drag-to-reorder persists the WHOLE fleet order from a drag the operator made
// inside a filtered view. If the write-back slips, nodes the filter had hidden
// silently move too — an order nobody asked for, saved to the server.

it("moves a row down when nothing is filtered out", () => {
  expect(reorderWithFilter(["a", "b", "c", "d"], ["a", "b", "c", "d"], 0, 2)).toEqual(["b", "c", "a", "d"]);
});

it("moves a row up when nothing is filtered out", () => {
  expect(reorderWithFilter(["a", "b", "c", "d"], ["a", "b", "c", "d"], 3, 1)).toEqual(["a", "d", "b", "c"]);
});

it("writes a filtered drag back into the visible slots only", () => {
  expect(reorderWithFilter(["a", "b", "c", "d", "e"], ["b", "d"], 1, 0)).toEqual(["a", "d", "c", "b", "e"]);
});

it("leaves hidden nodes on their own rows across a longer filtered drag", () => {
  const allIds = ["hidden1", "b", "hidden2", "d", "f", "hidden3"];
  expect(reorderWithFilter(allIds, ["b", "d", "f"], 0, 2)).toEqual(["hidden1", "d", "hidden2", "f", "b", "hidden3"]);
});

it("leaves the order alone for equal or out-of-range indices", () => {
  const allIds = ["a", "b", "c"];
  const visibleIds = ["a", "b", "c"];
  expect(reorderWithFilter(allIds, visibleIds, 1, 1)).toEqual(allIds);
  expect(reorderWithFilter(allIds, visibleIds, -1, 0)).toEqual(allIds);
  expect(reorderWithFilter(allIds, visibleIds, 0, 3)).toEqual(allIds);
  expect(reorderWithFilter(allIds, visibleIds, 5, 0)).toEqual(allIds);
});

it("leaves the order alone when a visible id is no longer in the fleet", () => {
  const allIds = ["a", "b"];
  expect(reorderWithFilter(allIds, ["a", "forgotten"], 1, 0)).toEqual(allIds);
});

it("does not mutate its inputs", () => {
  const allIds = ["a", "b", "c", "d", "e"];
  const visibleIds = ["b", "d"];
  reorderWithFilter(allIds, visibleIds, 1, 0);
  expect(allIds).toEqual(["a", "b", "c", "d", "e"]);
  expect(visibleIds).toEqual(["b", "d"]);
});
