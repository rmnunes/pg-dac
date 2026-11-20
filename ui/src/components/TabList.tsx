import type { ReactNode } from "react";

export interface TabSpec<K extends string> {
  key: K;
  label: string;
  count?: number;
}

/** The id of a tab and of the panel it controls, for aria-controls / aria-labelledby. */
export const tabId = (base: string, key: string): string => `${base}-tab-${key}`;
export const panelId = (base: string, key: string): string => `${base}-panel-${key}`;

// An ARIA tab list with roving focus: Left/Right move to the previous/next tab
// (wrapping), Home/End to the first/last, and moving selects (automatic
// activation, as the WAI-ARIA tabs pattern recommends for cheap panels).
export function TabList<K extends string>({
  base,
  tabs,
  active,
  onSelect,
  label,
  className,
  children,
}: {
  base: string;
  tabs: TabSpec<K>[];
  active: K;
  onSelect: (key: K) => void;
  label: string;
  className: string;
  children?: ReactNode;
}) {
  const move = (key: K) => {
    onSelect(key);
    document.getElementById(tabId(base, key))?.focus();
  };
  const onKeyDown = (e: React.KeyboardEvent) => {
    const i = tabs.findIndex((t) => t.key === active);
    const target =
      e.key === "ArrowRight"
        ? tabs[(i + 1) % tabs.length]
        : e.key === "ArrowLeft"
          ? tabs[(i - 1 + tabs.length) % tabs.length]
          : e.key === "Home"
            ? tabs[0]
            : e.key === "End"
              ? tabs[tabs.length - 1]
              : undefined;
    if (!target) return;
    e.preventDefault();
    move(target.key);
  };
  return (
    <div className={className} role="tablist" aria-label={label} onKeyDown={onKeyDown}>
      {tabs.map((t) => (
        <button
          key={t.key}
          type="button"
          id={tabId(base, t.key)}
          role="tab"
          className="tab"
          aria-selected={active === t.key}
          aria-controls={panelId(base, t.key)}
          tabIndex={active === t.key ? 0 : -1}
          onClick={() => onSelect(t.key)}
        >
          {t.label}
          {t.count ? <span className="count-pill">{t.count}</span> : null}
        </button>
      ))}
      {children}
    </div>
  );
}
