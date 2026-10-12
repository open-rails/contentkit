// Headless React layer: ContentKitProvider and every hook. No styling or strings.
// Hooks read the client from the provider; a `client` option overrides it.
export { ContentKitProvider, type ContentKitProviderProps } from "./provider.js";
export {
  useContentKit,
  useContentKitClient,
  useContentURLs,
  useErrorReporter,
  useGuarded,
  GuardContext,
  useOptionalContentKitClient,
  type ContentKitContextValue,
  type ContentKitErrorHandler,
  type ContentKitErrorInfo,
  type ContentKitGuard,
  type ContentKitOperation,
  type Navigate,
  type SocialOperation,
} from "./context.js";
export { useMediaRead, type MediaReadOptions, type ReadWindow, type UseMediaRead } from "./read.js";
export { nameIn, thumbnailOf, useMediaFolder, type FolderGroup, type MediaFolderOptions, type UseMediaFolder } from "./folder.js";
export { useEditorCrop, type EditorCropOptions, type EditorCropState, type UseEditorCrop } from "./crop.js";
export { usePresets, usePublicImage, type PublicImageOptions, type UsePresets, type UsePublicImage } from "./public.js";
export { useCanonicalContent, type CanonicalContent, type CanonicalContentOptions } from "./canonical.js";
export { useNearViewport, type NearViewportOptions } from "./viewport.js";
export {
  useAdminLocation,
  useFlowClient,
  useStaffAccess,
  type AdminLocation,
  type AdminNavigation,
  type AdminQuery,
  type ContentKitAuth,
  type FlowClientOptions,
  type StaffAccess,
  type StaffArea,
} from "./admin.js";
export {
  useAdminComments,
  useCanComment,
  useCommentBans,
  useGlobalCommentBan,
  useCommentReplies,
  useComments,
  useLatestComments,
  useModerationQueue,
  withReaction,
  type UseAdminComments,
  type UseCommentBans,
  type UseGlobalCommentBan,
  type UseComments,
  type UseCommentsOptions,
  type UseModerationQueue,
} from "./comments.js";
export { useContentConfig, type UseContentConfig } from "./config.js";
export { useFavorite, useReaction, type UseFavorite, type UseReaction } from "./engagement.js";
export { usePoll, usePollEditor, usePolls, withVote, type PollFilter, type PollImages, type UsePoll, type UsePollEditor } from "./polls.js";
export { isPublished, usePost, usePosts, type PostFilter, type UsePost } from "./posts.js";
export {
  memberId,
  useContentGroups,
  useGroup,
  useGroupMembers,
  useGroups,
  useKinds,
  type GroupFilter,
  type GroupSort,
  type Member,
  type UseContentGroups,
  type UseGroup,
  type UseGroupMembers,
  type UseKinds,
} from "./groups.js";
export {
  beaconsFor,
  useBeacons,
  useContentFilters,
  useContents,
  useExposure,
  useHistory,
  useRecommendations,
  useReleaseDates,
  useSearch,
  useSimilar,
  useTypeahead,
  useViewSignal,
  type ExposureOptions,
  type ReleaseDate,
  type UseContentFilters,
  type UseExposure,
  type UseHistory,
  type UseRail,
  type UseTypeahead,
} from "./discovery.js";
export {
  nextRelease,
  useReaderPlan,
  useSegmentPages,
  useVersionDownload,
  type ReaderPage,
  type UseReaderPlan,
  type UseSegmentPages,
  type UseVersionDownload,
} from "./versions.js";
export { useContentScope, usePaged, type UseList, type UsePaged } from "./use-resource.js";
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

export { useNotifications, useNotificationUnread, type UseNotifications, type UseNotificationsOptions } from "./notifications.js";
