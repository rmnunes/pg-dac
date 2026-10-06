import { useEffect, useId, useRef, type ReactNode } from "react";
import { X } from "lucide-react";

// A modal dialog on the native <dialog> element: the browser provides the top
// layer, the focus trap, Escape (the `cancel` event) and the backdrop. The
// component is mounted while open — unmounting closes it and returns focus to
// the element that was focused before it opened.
export function Dialog({
  title,
  onClose,
  children,
  className,
  width,
  dismissable = true,
  headerExtra,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  className?: string;
  width?: number | string;
  /** When false, Escape and the backdrop do nothing (a running operation). */
  dismissable?: boolean;
  headerExtra?: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    const opener = document.activeElement as HTMLElement | null;
    if (!d.open) d.showModal();
    return () => {
      if (d.open) d.close();
      opener?.focus?.();
    };
  }, []);

  return (
    <dialog
      ref={ref}
      className={"dialog" + (className ? " " + className : "")}
      style={width !== undefined ? { width } : undefined}
      aria-labelledby={titleId}
      onCancel={(e) => {
        e.preventDefault();
        if (dismissable) onClose();
      }}
      onClick={(e) => {
        if (dismissable && e.target === e.currentTarget) onClose();
      }}
    >
      <div className="dialog-body">
        <header className="dialog-header">
          <h2 id={titleId}>{title}</h2>
          {headerExtra}
          {dismissable && (
            <button type="button" className="icon-btn" aria-label="Close dialog" onClick={onClose}>
              <X size={16} aria-hidden="true" />
            </button>
          )}
        </header>
        {children}
      </div>
    </dialog>
  );
}
