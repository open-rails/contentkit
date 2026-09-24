import "./styles.css";

export { EncodeProgress, encodeLabel, formatRemaining, type EncodeProgressProps } from "./components/encode-progress.js";
export { ImageCropDialog, type ImageCropDialogProps } from "./components/image-crop-dialog.js";
export { SlotImage, type SlotImageProps } from "./components/slot-image.js";
export { DensityContext, RenditionImg, useRendition, type RenditionImgProps, type UseRenditionOptions } from "./components/rendition-img.js";
export {
  SlotEditError,
  SlotEditMenu,
  SlotEditor,
  useSlotEditor,
  type SlotEditMenuProps,
  type SlotEditorProps,
  type SlotEditorState,
} from "./components/slot-editor.js";
export { HoverPreview, VideoPoster, useReducedMotion, type HoverPreviewProps, type HoverPreviewSource, type VideoPosterProps } from "./components/video-poster.js";
export { VideoPosterPicker, formatTime, type VideoPickerProps, type VideoPosterPickerProps } from "./components/video-poster-picker.js";
export { HoverPreviewPicker, type HoverPreviewPickerProps } from "./components/hover-preview-picker.js";
export { MediaGallery, type MediaGalleryProps } from "./components/media-gallery.js";
export { SpriteFrame, VideoPlayer, qualityLabel, type VideoPlayerProps } from "./components/video-player.js";
export { AvatarUpload, CoverUpload, type SlotUploadProps } from "./components/slot-upload.js";
export { UploadUiProvider, useErrorReporter, useUploadClient, type UploadUiErrorHandler, type UploadUiOperation, type UploadUiProviderProps } from "./provider.js";
export { UploadUiRoot } from "./scope.js";
export type { UploadUiAppearance, UploadUiTheme, UploadUiVariables } from "./appearance.js";
export {
  createTranslator,
  defaultMessages,
  defineMessages,
  resolveMessages,
  type MessageKey,
  type MessageVars,
  type Translator,
  type UploadUiMessageBundle,
  type UploadUiMessages,
  type UploadUiTranslate,
} from "./i18n/messages.js";
export { useMessages } from "./i18n/context.js";
