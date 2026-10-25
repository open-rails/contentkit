import { useId } from "react";
import { attributeInput } from "../../client/entity-fields.js";
import type { Attribute } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { labelOf, type Labels } from "./parts.js";

const NONE = "__none__";

/** A kind's declared attributes as form fields. */
export function AttributeFields({ declared, form, onChange, labels }: { declared: Readonly<Record<string, Attribute>>; form: Record<string, string | boolean>; onChange: (name: string, v: string | boolean) => void; labels?: Labels }) {
  const { t } = useMessages();
  const id = useId();
  const names = Object.keys(declared).sort();
  if (!names.length) return null;
  return (
    // Two columns only when its own box is wide enough, so a side panel stacks them.
    <div className="@container">
      <div className="grid gap-3 @md:grid-cols-2" data-ckui="entity-attributes-form">
        {names.map((name) => {
          const spec = declared[name]!;
          const label = labelOf(labels, name);
          const value = form[name] ?? attributeInput(spec, undefined);
          if (spec.type === "bool")
            return (
              <div key={name} className="flex items-center gap-2">
                <Switch id={`${id}-${name}`} checked={value === true} onCheckedChange={(v) => onChange(name, v)} aria-label={label} />
                <Label htmlFor={`${id}-${name}`}>{label}</Label>
              </div>
            );
          if (spec.type === "enum")
            return (
              <div key={name} className="grid gap-1.5">
                <Label>{label}{spec.required && " *"}</Label>
                <Select value={String(value) || NONE} required={spec.required} onValueChange={(v) => onChange(name, v === NONE ? "" : String(v))}>
                  <SelectTrigger size="sm" aria-label={label} aria-required={spec.required}>
                    <SelectValue>{(v: string) => (v === NONE ? (spec.required ? t("entityEditor.choose") : t("entityEditor.none")) : labelOf(labels, v))}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {!spec.required && <SelectItem value={NONE}>{t("entityEditor.none")}</SelectItem>}
                    {(spec.values ?? []).map((v) => (
                      <SelectItem key={v} value={v}>
                        {labelOf(labels, v)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            );
          return (
            <div key={name} className="grid gap-1.5">
              <Label htmlFor={`${id}-${name}`}>{label}{spec.required && " *"}</Label>
              <Input
                id={`${id}-${name}`}
                required={spec.required}
                className="h-8"
                type={spec.type === "int" ? "number" : spec.type === "time" ? "datetime-local" : "text"}
                step={spec.type === "int" ? 1 : undefined}
                value={String(value)}
                onChange={(e) => onChange(name, e.target.value)}
              />
            </div>
          );
        })}
      </div>
    </div>
  );
}

/** The form values of stored attributes. */
export const formOf = (declared: Readonly<Record<string, Attribute>>, values: Record<string, unknown>) =>
  Object.fromEntries(Object.entries(declared).map(([n, s]) => [n, attributeInput(s, values[n])]));

