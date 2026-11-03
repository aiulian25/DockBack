// ScrollToTop: a "back to top" button once a page is scrolled down, and every
// new page starting at its top. Pages scroll inside the layout's <main>, not the
// window, so both act on that element.
import { RefObject, useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { ArrowUp } from "lucide-react";

// About a screen's worth down: before that, the top is one flick away anyway.
const SHOW_AFTER_PX = 400;

const reducedMotion = () => window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;

export default function ScrollToTop({ target }: { target: RefObject<HTMLElement | null> }) {
  const [shown, setShown] = useState(false);
  const { pathname, hash } = useLocation();

  // A new page opens at its top, not where the previous one was left. A link to
  // a section (#schedule) is left to the page, which scrolls to it itself.
  useEffect(() => {
    if (!hash) target.current?.scrollTo(0, 0);
  }, [pathname, hash, target]);

  useEffect(() => {
    const el = target.current;
    if (!el) return;
    const update = () => setShown(el.scrollTop > SHOW_AFTER_PX);
    update();
    el.addEventListener("scroll", update, { passive: true });
    return () => el.removeEventListener("scroll", update);
  }, [target]);

  if (!shown) return null;
  return (
    <button
      type="button"
      onClick={() => target.current?.scrollTo({ top: 0, behavior: reducedMotion() ? "auto" : "smooth" })}
      aria-label="Back to top"
      title="Back to top"
      className="fixed bottom-6 right-6 z-40 grid h-10 w-10 place-items-center rounded-full border border-outline-variant bg-surface-high text-on-surface shadow-lg transition-colors hover:bg-docker-blue hover:text-white focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary"
    >
      <ArrowUp size={18} />
    </button>
  );
}
