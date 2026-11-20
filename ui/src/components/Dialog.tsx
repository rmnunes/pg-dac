import { useEffect, useId, useRef, type ReactNode } from "react";
import { X } from "lucide-react";

// A modal dialog on the native <dialog> element: the browser provides the top
// layer, the focus trap, Escape (the `cancel` event) and the backdrop. The
// component is mounted while open — unmounting closes it and returns focus to
// the element that was focused before it opened. The component, not the
// browser, decides when the dialog goes away: Chromium's close watcher closes
// a dialog on a repeated Escape even when `cancel` is prevented, so a native
// `close` the component did not ask for reopens the dialog.
export function Dialog({
  title,
  onClose,
  onEscape,
  children,
  className,
  width,
  dismissable = true,
  backdropCloses = dismissable,
  headerExtra,
}: {
  title: string;
  onClose: () => void;
  /** What Escape does; defaults to onClose. */
  onEscape?: () => void;
  children: ReactNode;
  className?: string;
  width?: number | string;
  /** When false, Escape, the backdrop and the close button do nothing (a running operation). */
  dismissable?: boolean;
  /** Whether a click on the backdrop closes the dialog (defaults to dismissable). */
  backdropCloses?: boolean;
  headerExtra?: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const unmounting = useRef(false);
  const titleId = useId();
  const escape = () => {
    if (dismissable) (onEscape ?? onClose)();
  };

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    unmounting.current = false;
    const opener = document.activeElement as HTMLElement | null;
    if (!d.open) d.showModal();
    return () => {
      unmounting.current = true;
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
        escape();
      }}
      onClose={() => {
        // The event is queued: by the time it runs the dialog may be open again
        // (StrictMode re-runs the mount effect, which closes and reopens it).
        // Only a close that is still in effect is the browser's.
        if (unmounting.current || ref.current?.open) return;
        // The browser closed it on its own (see above): treat it as Escape,
        // and keep it open unless that unmounts the component.
        escape();
        requestAnimationFrame(() => {
          const d = ref.current;
          if (!unmounting.current && d && !d.open) d.showModal();
        });
      }}
      onClick={(e) => {
        if (backdropCloses && e.target === e.currentTarget) onClose();
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
