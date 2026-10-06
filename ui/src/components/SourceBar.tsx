import { ArrowLeftRight } from "lucide-react";

import { useStore } from "../state/store";
import { SourcePicker } from "./SourcePicker";

// SSDT Schema Compare orientation: Source (of truth) on the LEFT, Target (gets
// updated) on the RIGHT; a publish pushes Source → Target. Source is the
// engine's --to (desired); Target is --from (the side a publish applies to).
export function SourceBar() {
  const swap = useStore((s) => s.swap);
  return (
    <section className="sourcebar" aria-label="Sources">
      <SourcePicker side="to" label="Source" hint="the desired state" />
      <button type="button" className="icon-btn swap" title="Swap Source and Target" aria-label="Swap Source and Target" onClick={swap}>
        <ArrowLeftRight size={16} aria-hidden="true" />
      </button>
      <SourcePicker side="from" label="Target" hint="gets updated to match the Source" />
    </section>
  );
}
