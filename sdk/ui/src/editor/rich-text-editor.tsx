import {
  ArrowTurnBackwardIcon,
  ArrowTurnForwardIcon,
  Heading02Icon,
  Heading03Icon,
  Image01Icon,
  LeftToRightListBulletIcon,
  LeftToRightListNumberIcon,
  Link01Icon,
  QuoteDownIcon,
  TextBoldIcon,
  TextItalicIcon,
  TextStrikethroughIcon,
  TextUnderlineIcon,
} from "@hugeicons/core-free-icons";
import { HugeiconsIcon, type IconSvgElement } from "@hugeicons/react";
import Image from "@tiptap/extension-image";
import { Placeholder } from "@tiptap/extensions";
import { EditorContent, useEditor, useEditorState, type Editor } from "@tiptap/react";
import StarterKit from "@tiptap/starter-kit";
import { cn } from "cn";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { ContentEditorProps } from "../components/admin/content-manager.js";
import { useMessages } from "../i18n/context.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { ErrorLine } from "../components/social/parts.js";

export type RichTextEditorProps = ContentEditorProps & {
  className?: string;
};

const content =
  "min-h-48 px-3 py-2 outline-none break-words [&_p]:my-2 [&_h2]:mt-4 [&_h2]:mb-2 [&_h2]:text-lg [&_h2]:font-semibold [&_h3]:mt-3 [&_h3]:mb-1.5 [&_h3]:font-semibold " +
  "[&_ul]:my-2 [&_ul]:list-disc [&_ul]:pl-6 [&_ol]:my-2 [&_ol]:list-decimal [&_ol]:pl-6 [&_blockquote]:my-2 [&_blockquote]:border-l-2 [&_blockquote]:border-border [&_blockquote]:pl-3 [&_blockquote]:text-muted-foreground " +
  "[&_a]:text-primary [&_a]:underline [&_img]:my-2 [&_img]:max-h-96 [&_img]:max-w-full [&_img]:rounded-md [&_img.ProseMirror-selectednode]:ring-2 [&_img.ProseMirror-selectednode]:ring-ring " +
  "[&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_pre]:my-2 [&_pre]:rounded-md [&_pre]:bg-muted [&_pre]:p-3 " +
  "[&_p.is-editor-empty:first-child]:before:pointer-events-none [&_p.is-editor-empty:first-child]:before:float-left [&_p.is-editor-empty:first-child]:before:h-0 [&_p.is-editor-empty:first-child]:before:text-muted-foreground [&_p.is-editor-empty:first-child]:before:content-[attr(data-placeholder)]";

/**
 * A rich-text field over Tiptap: headings, emphasis, lists, quotes, links
 * and images (picked, pasted or dropped, through uploadImage). Its value is
 * HTML.
 */
export function RichTextEditor({ value, onChange, uploadImage, disabled, placeholder, className, ...aria }: RichTextEditorProps) {
  const { t } = useMessages();
  const [error, setError] = useState<ContentKitError | null>(null);
  const [uploading, setUploading] = useState(0);
  const upload = useRef(uploadImage);
  const change = useRef(onChange);
  useEffect(() => {
    upload.current = uploadImage;
    change.current = onChange;
  });
  const insertImages = async (ed: Editor, files: File[], pos?: number) => {
    if (!upload.current) return;
    setError(null);
    for (const file of files) {
      setUploading((n) => n + 1);
      try {
        const src = await upload.current(file);
        const chain = ed.chain().focus();
        (pos === undefined ? chain : chain.setTextSelection(pos)).setImage({ src, alt: "" }).run();
      } catch (e) {
        setError(toContentKitError(e));
      } finally {
        setUploading((n) => n - 1);
      }
    }
  };
  const images = (list: FileList | null | undefined) => [...(list ?? [])].filter((f) => f.type.startsWith("image/"));
  const editor = useEditor({
    extensions: [
      StarterKit.configure({ heading: { levels: [2, 3] }, link: { openOnClick: false, autolink: true, defaultProtocol: "https" } }),
      Image.configure({ inline: false }),
      Placeholder.configure({ placeholder: placeholder ?? t("editor.placeholder") }),
    ],
    content: value,
    editable: !disabled,
    immediatelyRender: false,
    editorProps: {
      attributes: { class: content, role: "textbox", "aria-multiline": "true", ...(aria["aria-label"] ? { "aria-label": aria["aria-label"] } : {}) },
      handlePaste: (view, event) => {
        const files = images(event.clipboardData?.files);
        if (!files.length || !upload.current || !editorRef.current) return false;
        void insertImages(editorRef.current, files);
        return true;
      },
      handleDrop: (view, event) => {
        const files = images((event as DragEvent).dataTransfer?.files);
        if (!files.length || !upload.current || !editorRef.current) return false;
        const at = view.posAtCoords({ left: event.clientX, top: event.clientY });
        void insertImages(editorRef.current, files, at?.pos);
        return true;
      },
    },
    onUpdate: ({ editor }) => change.current(editor.isEmpty ? "" : editor.getHTML()),
  });
  const editorRef = useRef<Editor | null>(null);
  editorRef.current = editor;
  // A value from outside (a load, a reset) replaces the document.
  useEffect(() => {
    if (!editor) return;
    const html = editor.isEmpty ? "" : editor.getHTML();
    if (value !== html) editor.commands.setContent(value, { emitUpdate: false });
  }, [editor, value]);
  useEffect(() => {
    editor?.setEditable(!disabled);
  }, [editor, disabled]);
  return (
    <div className={cn("grid overflow-hidden rounded-lg border border-input bg-background focus-within:ring-2 focus-within:ring-ring/50", className)} data-ckui="rich-text-editor">
      {editor && <Toolbar editor={editor} disabled={disabled} canUpload={!!uploadImage} uploading={uploading > 0} onImages={(files) => void insertImages(editor, files)} />}
      <EditorContent editor={editor} />
      {error && <ErrorLine error={error} className="px-3 pb-2" />}
      {uploading > 0 && (
        <p className="px-3 pb-2 text-xs text-muted-foreground" role="status">
          {t("editor.uploading")}
        </p>
      )}
    </div>
  );
}

/** A render prop for ContentManager: `renderEditor={richTextEditor}`. */
export function richTextEditor(props: ContentEditorProps): ReactNode {
  return <RichTextEditor {...props} />;
}

function Toolbar({ editor, disabled, canUpload, uploading, onImages }: { editor: Editor; disabled?: boolean; canUpload: boolean; uploading: boolean; onImages: (files: File[]) => void }) {
  const { t } = useMessages();
  const id = useId();
  const file = useRef<HTMLInputElement>(null);
  const [linking, setLinking] = useState<string | null>(null);
  const state = useEditorState({
    editor,
    selector: ({ editor: e }) => ({
      bold: e.isActive("bold"),
      italic: e.isActive("italic"),
      underline: e.isActive("underline"),
      strike: e.isActive("strike"),
      h2: e.isActive("heading", { level: 2 }),
      h3: e.isActive("heading", { level: 3 }),
      bullet: e.isActive("bulletList"),
      ordered: e.isActive("orderedList"),
      quote: e.isActive("blockquote"),
      link: e.isActive("link"),
      href: (e.getAttributes("link").href as string | undefined) ?? "",
      undo: e.can().undo(),
      redo: e.can().redo(),
    }),
  });
  const tool = (label: string, icon: IconSvgElement, active: boolean | undefined, run: () => void, enabled = true) => (
    <Button
      key={label}
      type="button"
      size="icon-sm"
      variant="ghost"
      className={cn(active && "bg-accent text-accent-foreground")}
      aria-label={label}
      title={label}
      aria-pressed={active === undefined ? undefined : active}
      disabled={disabled || !enabled}
      onMouseDown={(e) => e.preventDefault()}
      onClick={run}
    >
      <HugeiconsIcon icon={icon} />
    </Button>
  );
  const c = () => editor.chain().focus();
  const saveLink = () => {
    const href = linking?.trim();
    if (href) c().extendMarkRange("link").setLink({ href }).run();
    else c().extendMarkRange("link").unsetLink().run();
    setLinking(null);
  };
  return (
    <div className="grid gap-1 border-b border-border bg-muted/40 p-1">
      <div className="flex flex-wrap items-center gap-0.5" role="toolbar" aria-label={t("editor.toolbar")}>
        {tool(t("editor.bold"), TextBoldIcon, state.bold, () => c().toggleBold().run())}
        {tool(t("editor.italic"), TextItalicIcon, state.italic, () => c().toggleItalic().run())}
        {tool(t("editor.underline"), TextUnderlineIcon, state.underline, () => c().toggleUnderline().run())}
        {tool(t("editor.strike"), TextStrikethroughIcon, state.strike, () => c().toggleStrike().run())}
        <span className="mx-1 h-5 w-px bg-border" aria-hidden />
        {tool(t("editor.heading2"), Heading02Icon, state.h2, () => c().toggleHeading({ level: 2 }).run())}
        {tool(t("editor.heading3"), Heading03Icon, state.h3, () => c().toggleHeading({ level: 3 }).run())}
        {tool(t("editor.bulletList"), LeftToRightListBulletIcon, state.bullet, () => c().toggleBulletList().run())}
        {tool(t("editor.orderedList"), LeftToRightListNumberIcon, state.ordered, () => c().toggleOrderedList().run())}
        {tool(t("editor.quote"), QuoteDownIcon, state.quote, () => c().toggleBlockquote().run())}
        <span className="mx-1 h-5 w-px bg-border" aria-hidden />
        {tool(t("editor.link"), Link01Icon, state.link, () => setLinking(state.href))}
        {canUpload && tool(t("editor.image"), Image01Icon, undefined, () => file.current?.click(), !uploading)}
        <span className="mx-1 h-5 w-px bg-border" aria-hidden />
        {tool(t("editor.undo"), ArrowTurnBackwardIcon, undefined, () => c().undo().run(), state.undo)}
        {tool(t("editor.redo"), ArrowTurnForwardIcon, undefined, () => c().redo().run(), state.redo)}
        <input
          ref={file}
          type="file"
          accept="image/*"
          multiple
          hidden
          aria-label={t("editor.image")}
          onChange={(e) => {
            const files = [...(e.target.files ?? [])];
            e.target.value = "";
            if (files.length) onImages(files);
          }}
        />
      </div>
      {linking !== null && (
        <form
          className="flex flex-wrap items-center gap-2 px-1 pb-1"
          onSubmit={(e) => {
            e.preventDefault();
            saveLink();
          }}
        >
          <label htmlFor={`${id}-href`} className="sr-only">
            {t("editor.linkUrl")}
          </label>
          <Input id={`${id}-href`} className="h-8 min-w-48 flex-1" type="url" placeholder="https://" value={linking} autoFocus onChange={(e) => setLinking(e.target.value)} />
          <Button type="submit" size="sm">
            {t(linking.trim() ? "editor.applyLink" : "editor.removeLink")}
          </Button>
          <Button type="button" size="sm" variant="ghost" onClick={() => setLinking(null)}>
            {t("common.cancel")}
          </Button>
        </form>
      )}
    </div>
  );
}
