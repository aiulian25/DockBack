// Reordering the fleet while a CLUSTER FILTER is on.
//
// The Servers page persists ONE order for the whole fleet, but the operator
// drags inside whatever the filter left on screen. Splicing the full list would
// therefore drag the hidden nodes around too. Instead the visible nodes are
// reordered among themselves and written back into exactly the slots they
// already occupied, so every filtered-out node keeps its row.

export function reorderWithFilter(allIds: string[], visibleIds: string[], fromIndex: number, toIndex: number): string[] {
  if (fromIndex === toIndex) return allIds;
  if (fromIndex < 0 || fromIndex >= visibleIds.length) return allIds;
  if (toIndex < 0 || toIndex >= visibleIds.length) return allIds;

  const visible = new Set(visibleIds);
  const visibleSlots = allIds.flatMap((id, index) => (visible.has(id) ? index : []));
  // Fewer slots than visible ids means the two lists have drifted apart (a node
  // forgotten mid-drag, a duplicate id). Writing back would then shift rows into
  // slots that belong to someone else, so the order is left alone.
  if (visibleSlots.length !== visibleIds.length) return allIds;

  const moved = [...visibleIds];
  const [dragged] = moved.splice(fromIndex, 1);
  moved.splice(toIndex, 0, dragged);

  const next = [...allIds];
  visibleSlots.forEach((slot, position) => { next[slot] = moved[position]; });
  return next;
}
