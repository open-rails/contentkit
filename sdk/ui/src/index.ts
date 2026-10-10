// @openrails/contentkit-ui: styled ContentKit components, ContentKitUiProvider
// and i18n. Components that talk to ContentKit need ContentKitProvider from
// `./react`. The entry installs its isolated stylesheet once in browsers.
import "./styles.css";

export { ContentKitUiProvider, type ContentKitUiProviderProps } from "./provider.js";
export { ContentKitUiRoot } from "./scope.js";
export type { ContentKitUiAppearance, ContentKitUiTheme, ContentKitUiVariables } from "./appearance.js";
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
export { VideoPoster, type VideoPosterProps } from "./components/video-poster.js";
export { VideoPosterPicker, formatTime, type VideoPickerProps, type VideoPosterPickerProps } from "./components/video-poster-picker.js";
export { MediaGallery, type MediaGalleryProps } from "./components/media-gallery.js";
export {
  SpriteFrame,
  VideoPlayer,
  qualityLabel,
  type PlayerEvent,
  type PlayerHandoff,
  type PlayerMenuItem,
  type PlayerSlotContext,
  type VideoPlayerProps,
} from "./components/video-player.js";
export { VideoMiniPlayer, type VideoMiniPlayerProps } from "./components/video-mini-player.js";
export { playerButtonClass } from "./components/player-controls.js";
export { AvatarUpload, CoverUpload, type SlotUploadProps } from "./components/slot-upload.js";
export * from "./components/social/index.js";
export {
  createTranslator,
  defaultMessages,
  defineMessages,
  resolveMessages,
  type MessageKey,
  type MessageVars,
  type Translator,
  type ContentKitUiMessageBundle,
  type ContentKitUiMessages,
  type ContentKitUiTranslate,
} from "./i18n/messages.js";
export { useMessages } from "./i18n/context.js";
