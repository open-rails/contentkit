import "./styles.css";

export { ImageCropDialog, type ImageCropDialogProps } from "./components/image-crop-dialog.js";
export { SlotImage, type SlotImageProps } from "./components/slot-image.js";
export { AvatarUpload, CoverUpload, type SlotUploadProps } from "./components/slot-upload.js";
export { UploadUiProvider, useUploadClient, type UploadUiProviderProps } from "./provider.js";
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
