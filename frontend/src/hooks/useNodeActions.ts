import { useEffect, useRef, useState } from "react";
import { api, Node } from "../api";
import { useToast } from "../components/Toast";
import { reorderWithFilter } from "../lib/nodeOrder";

interface NodeActionsOptions {
  nodes: Node[] | null;
  setNodes: (nodes: Node[]) => void;
  /** Refetch after a forget settles, so the list matches the server. */
  reload: () => void;
}

const FORGET_CONFIRMATION = (name: string) =>
  `Forget node "${name}"?\n\n` +
  `This permanently deletes ALL of this node's backups (local and offsite) and its stored connection credentials. ` +
  `It does not touch the host itself. You'll have a few seconds to undo before the data is purged.`;

/**
 * useNodeActions owns the two things the node list and the node cards both do:
 * forgetting a node, and dragging one into a new position.
 *
 * Both pages had their own copy. Forgetting is destructive — it deletes every
 * backup the node holds — so the confirmation text, the immediate server-side
 * commit and the undo window are exactly the parts that must not be allowed to
 * drift between two views of the same fleet.
 */
export function useNodeActions({ nodes, setNodes, reload }: NodeActionsOptions) {
  const toast = useToast();

  // Forgotten nodes are hidden optimistically while the undo window runs.
  const [pendingDelete, setPendingDelete] = useState<Set<string>>(new Set());

  // A card or row is draggable only while its grip is held: a permanently
  // draggable element swallows text selection, and the endpoint cell is
  // deliberately selectable.
  const [grabbedNodeId, setGrabbedNodeId] = useState<string | null>(null);
  const [dragIndex, setDragIndex] = useState<number | null>(null);
  const [dropIndex, setDropIndex] = useState<number | null>(null);
  // A drop is followed by a click on whatever it landed on. That click is the
  // tail of the drag, not a request to open the node.
  const justDragged = useRef(false);
  const clearDrag = () => { setGrabbedNodeId(null); setDragIndex(null); setDropIndex(null); };

  useEffect(() => {
    if (!grabbedNodeId) return;
    // A press that never became a drag still has to disarm, or that element
    // keeps its draggable attribute and loses its selectable text.
    const disarm = () => setGrabbedNodeId(null);
    window.addEventListener("mouseup", disarm);
    return () => window.removeEventListener("mouseup", disarm);
  }, [grabbedNodeId]);

  /**
   * Forget a node. The deletion is committed to the server immediately so it is
   * durable — it cannot resurrect on the next poll or a refresh — and a short
   * undo restores it.
   */
  const forgetNode = async (node: Node) => {
    if (!confirm(FORGET_CONFIRMATION(node.name))) return;
    const unhide = () => setPendingDelete((current) => {
      const next = new Set(current);
      next.delete(node.id);
      return next;
    });
    setPendingDelete((current) => new Set(current).add(node.id));
    try {
      await api.deleteNode(node.id);
    } catch (error) {
      unhide();
      toast.error(`Couldn't remove node: ${(error as Error).message}`);
      return;
    }
    toast.undo({
      message: `Forgot node "${node.name}"`,
      onUndo: async () => {
        try { await api.restoreNode(node.id); }
        catch (error) { toast.error(`Couldn't restore node: ${(error as Error).message}`); }
        finally { unhide(); reload(); }
      },
      onCommit: () => { unhide(); reload(); }, // already deleted server-side; just reconcile
    });
  };

  /**
   * Move a node within the currently visible subset.
   *
   * The arrangement is the operator's own, so it lands instantly and only
   * flinches if the server refuses it. When a cluster filter is showing a
   * subset, the dragged node moves within that subset and the hidden ones keep
   * their places.
   */
  const reorderNodes = async (visibleIds: string[], fromIndex: number, toIndex: number) => {
    const previous = nodes;
    if (!previous || fromIndex === toIndex) return;
    const order = reorderWithFilter(previous.map((n) => n.id), visibleIds, fromIndex, toIndex);
    const nodeById = new Map(previous.map((n) => [n.id, n]));
    setNodes(order.flatMap((id) => nodeById.get(id) ?? []));
    try { await api.saveNodeOrder(order); }
    catch (error) { setNodes(previous); toast.error(`Couldn't save the new order: ${(error as Error).message}`); }
  };

  return {
    pendingDelete, forgetNode,
    grabbedNodeId, setGrabbedNodeId,
    dragIndex, setDragIndex,
    dropIndex, setDropIndex,
    justDragged, clearDrag, reorderNodes,
  };
}
