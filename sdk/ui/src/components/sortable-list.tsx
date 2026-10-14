import {
  DndContext,
  DragOverlay,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type Announcements,
  type ScreenReaderInstructions,
} from "@dnd-kit/core";
import { SortableContext, sortableKeyboardCoordinates, useSortable } from "@dnd-kit/sortable";
import { DragDropVerticalIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useMemo, useState, type HTMLAttributes, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../appearance.js";
import { useMessages } from "../i18n/context.js";
import { ContentKitUiRoot } from "../scope.js";

export interface SortableListProps<T> {
  items: readonly T[];
  /** A stable key per item. */
  id: (item: T) => string;
  /** The item's spoken name in announcements and the handle's label. */
  name: (item: T) => string;
  /** Called once per drop with the old and new index. */
  onMove: (from: number, to: number) => void;
  /** Rows (default) or a wrapping grid of tiles. */
  layout?: "list" | "grid";
  disabled?: boolean;
  /** Attributes of an item's `<li>`. */
  row?: (item: T) => HTMLAttributes<HTMLLIElement> & Record<`data-${string}`, unknown>;
  /** An item's content; place `handle` (its drag handle) where it belongs. */
  children: (item: T, handle: ReactNode, index: number) => ReactNode;
  className?: string;
  /** Accessible name of the list. */
  label?: string;
  appearance?: ContentKitUiAppearance;
}

// Items stay put while dragging; a bar marks where the item will land.
const noShift = () => null;
const handleClass =
  "inline-flex size-8 shrink-0 cursor-grab touch-none items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring active:cursor-grabbing disabled:cursor-default disabled:opacity-50";

/**
 * A reorderable list or grid (dnd-kit): drag an item by its handle with a
 * mouse, a finger or the keyboard (Space or Enter lifts, arrows move, Space
 * or Enter drops, Escape cancels), with localized screen reader
 * announcements. onMove gets the old and new index once, on drop.
 */
export function SortableList<T>({ items, id, name, onMove, layout = "list", disabled, row, children, className, label, appearance }: SortableListProps<T>) {
  const { t } = useMessages();
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }), useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }));
  const ids = useMemo(() => items.map(id), [items, id]);
  const [active, setActive] = useState<string | null>(null);
  const [over, setOver] = useState<string | null>(null);
  const at = (key: unknown) => ids.indexOf(String(key));
  const spoken = (key: unknown) => {
    const i = at(key);
    return i < 0 ? "" : name(items[i]!);
  };
  const total = ids.length;
  const announcements: Announcements = {
    onDragStart: ({ active }) => t("sortable.pickedUp", { name: spoken(active.id), position: at(active.id) + 1, total }),
    onDragOver: ({ active, over }) => (over ? t("sortable.over", { name: spoken(active.id), position: at(over.id) + 1, total }) : undefined),
    onDragEnd: ({ active, over }) =>
      over ? t("sortable.dropped", { name: spoken(active.id), position: at(over.id) + 1, total }) : t("sortable.droppedAway", { name: spoken(active.id) }),
    onDragCancel: ({ active }) => t("sortable.canceled", { name: spoken(active.id) }),
  };
  const instructions: ScreenReaderInstructions = { draggable: t("sortable.instructions") };
  const from = active ? at(active) : -1;
  const to = over ? at(over) : -1;
  const dragged = from >= 0 ? items[from] : undefined;
  const grid = layout === "grid";
  const list = (inner: ReactNode, overlay?: boolean) => (
    <ol
      className={cn(grid ? "grid grid-cols-[repeat(auto-fill,minmax(8rem,1fr))] gap-2" : "grid gap-1", className)}
      aria-label={overlay ? undefined : label}
      data-ckui="sortable-list"
      data-layout={layout}
      data-overlay={overlay ? "" : undefined}
    >
      {inner}
    </ol>
  );
  return (
    <ContentKitUiRoot appearance={appearance} className="contents">
      <DndContext
        sensors={sensors}
        collisionDetection={closestCenter}
        accessibility={{ announcements, screenReaderInstructions: instructions }}
        onDragStart={(e) => setActive(String(e.active.id))}
        onDragOver={(e) => setOver(e.over ? String(e.over.id) : null)}
        onDragCancel={() => {
          setActive(null);
          setOver(null);
        }}
        onDragEnd={(e) => {
          setActive(null);
          setOver(null);
          const a = at(e.active.id);
          const b = e.over ? at(e.over.id) : -1;
          if (a >= 0 && b >= 0 && a !== b) onMove(a, b);
        }}
      >
        <SortableContext items={ids} strategy={noShift} disabled={disabled}>
          {list(
            items.map((item, i) => (
              <Item
                key={ids[i]}
                id={ids[i]!}
                label={t("sortable.handle", { name: name(item) })}
                attrs={row?.(item)}
                disabled={disabled}
                grid={grid}
                dragging={i === from}
                drop={from >= 0 && i === to && to !== from ? (to > from ? "after" : "before") : undefined}
                render={(handle) => children(item, handle, i)}
              />
            )),
          )}
        </SortableContext>
        <DragOverlay dropAnimation={null}>
          {dragged !== undefined
            ? list(
                <li {...row?.(dragged)} className={cn(row?.(dragged).className, "cursor-grabbing rounded-md bg-background shadow-lg ring-1 ring-border")}>
                  {children(
                    dragged,
                    <span className={handleClass}>
                      <HugeiconsIcon icon={DragDropVerticalIcon} strokeWidth={2} className="size-4.5" />
                    </span>,
                    from,
                  )}
                </li>,
                true,
              )
            : null}
        </DragOverlay>
      </DndContext>
    </ContentKitUiRoot>
  );
}

function Item({
  id,
  label,
  attrs,
  disabled,
  grid,
  dragging,
  drop,
  render,
}: {
  id: string;
  label: string;
  attrs?: HTMLAttributes<HTMLLIElement>;
  disabled?: boolean;
  grid: boolean;
  dragging: boolean;
  drop?: "before" | "after";
  render: (handle: ReactNode) => ReactNode;
}) {
  const { attributes, listeners, setNodeRef, setActivatorNodeRef } = useSortable({ id });
  const handle = (
    <button type="button" ref={setActivatorNodeRef} className={handleClass} aria-label={label} disabled={disabled} data-ckui="sort-handle" {...attributes} {...listeners}>
      <HugeiconsIcon icon={DragDropVerticalIcon} strokeWidth={2} className="size-4.5" />
    </button>
  );
  return (
    <li
      {...attrs}
      ref={setNodeRef}
      className={cn(
        attrs?.className,
        "relative data-dragging:opacity-40 before:pointer-events-none before:absolute before:rounded-full before:bg-primary before:opacity-0",
        grid
          ? "before:inset-y-0 before:w-0.5 data-[drop=before]:before:-left-[5px] data-[drop=before]:before:opacity-100 data-[drop=after]:before:-right-[5px] data-[drop=after]:before:opacity-100"
          : "before:inset-x-0 before:h-0.5 data-[drop=before]:before:-top-[3px] data-[drop=before]:before:opacity-100 data-[drop=after]:before:-bottom-[3px] data-[drop=after]:before:opacity-100",
      )}
      data-drop={drop}
      data-dragging={dragging || undefined}
    >
      {render(handle)}
    </li>
  );
}
