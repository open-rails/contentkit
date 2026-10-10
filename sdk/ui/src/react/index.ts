// Headless React layer: ContentKitProvider and every hook. No styling or strings.
// Hooks read the client from the provider; a `client` option overrides it.
export { ContentKitProvider, type ContentKitProviderProps } from "./provider.js";
export {
  useContentKit,
  useContentKitClient,
  useContentURLs,
  useErrorReporter,
  useOptionalContentKitClient,
  type ContentKitContextValue,
  type ContentKitErrorHandler,
  type ContentKitOperation,
  type Navigate,
  type SocialOperation,
} from "./context.js";
export { useRead, type UseRead, type UseReadOptions } from "./read.js";
export {
  useAdminComments,
  useCanComment,
  useCommentBans,
  useCommentReplies,
  useComments,
  useLatestComments,
  useModerationQueue,
  withReaction,
  type UseAdminComments,
  type UseCommentBans,
  type UseComments,
  type UseCommentsOptions,
  type UseModerationQueue,
} from "./comments.js";
export { useFavorite, useReaction, type UseFavorite, type UseReaction } from "./engagement.js";
export { usePoll, usePollEditor, usePolls, withVote, type PollFilter, type PollImages, type UsePoll, type UsePollEditor } from "./polls.js";
export { usePost, usePosts, type PostFilter, type UsePost } from "./posts.js";
export { useContentScope, type UseList } from "./use-resource.js";
export { ResourceStore, resourcesFor, type Page, type Resource } from "./resources.js";
export {
  useCrop,
  useEncodeProgress,
  useUpload,
  useUploadQueue,
  type UploadQueueOptions,
  type UploadStatus,
  type UseCrop,
  type UseCropOptions,
  type UseEncodeProgress,
  type UseUpload,
  type UseUploadQueue,
} from "./upload.js";
export {
  editOutput,
  useSlotCrop,
  useSlotImage,
  type SlotCropMode,
  type SlotCropOptions,
  type SlotCropState,
  type SlotImageOptions,
  type UseSlotCrop,
  type UseSlotImage,
} from "./slot.js";
export {
  round3,
  useFrameStrip,
  useVideoFrame,
  useVideoImages,
  useVideoPoster,
  type FrameStripOptions,
  type StripFrame,
  type UseVideoFrame,
  type UseVideoImages,
  type UseVideoPoster,
  type VideoFrameOptions,
  type VideoImagesOptions,
  type VideoPosterOptions,
  type VideoSaveState,
} from "./video.js";
export {
  GALLERY_VIEW_KEY,
  PLAYER_QUALITY_KEY,
  useCarousel,
  useGalleryView,
  useHlsPlayer,
  useRefreshBeforeExpiry,
  type CarouselOptions,
  type GalleryViewOptions,
  type HlsPlayerOptions,
  type PlayerQuality,
  type PlayerStatus,
  type PlayerTracks,
  type QualityLevel,
  type UseCarousel,
  type UseHlsPlayer,
} from "./gallery.js";
export type { PlaybackProgress, ProgressReason } from "./player.js";
export { HOVER_DELAY, InlinePreviewContext, useInlinePreview, useReducedMotion, type InlinePreviewOptions } from "./inline-preview.js";
