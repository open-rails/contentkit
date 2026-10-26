// Headless React layer: ContentKitProvider and every hook. No styling or strings.
// Hooks read the client from the provider; a `client` option overrides it.
export { ContentKitProvider, type ContentKitProviderProps } from "./provider.js";
export { useChannel, useChannels, useMyChannels, useChannelContents } from "./channels.js";
export { useReaderPreferences } from "./reader-preferences.js";
export { usePageNotices, type UsePageNotices } from "./page-notices.js";
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
export { useAvatar, usePresets, usePublicImage, type PublicImageOptions, type UsePresets, type UsePublicImage } from "./public.js";
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
  useBannedWords,
  useCanComment,
  useCommentBans,
  useGlobalCommentBan,
  useIPBans,
  useCommentReplies,
  useComments,
  useLatestComments,
  useModerationQueue,
  withReaction,
  type UseAdminComments,
  type UseBannedWords,
  type UseIPBans,
  type UseCommentBans,
  type UseGlobalCommentBan,
  type UseComments,
  type UseCommentsOptions,
  type UseModerationQueue,
} from "./comments.js";
export { useContentConfig, type UseContentConfig } from "./config.js";
export { useFavorite, useReaction, type UseFavorite, type UseReaction } from "./engagement.js";
export { usePoll, usePollEditor, usePolls, withVote, type PollFilter, type PollImages, type UsePoll, type UsePollEditor } from "./polls.js";
export { useContent, useManagedContents, type ManageFilter, type UseContent } from "./contents.js";
export { useActivity, useMyStats, type UseStats } from "./stats.js";
export { defaultVersion, editionOf, findVersion, liveVersions, useContentDetail, type ContentDetailOptions, type UseContentDetail } from "./detail.js";
export { useVersionParam } from "./location.js";
export {
  memberId,
  useContentEntities,
  useEntity,
  useEntityMembers,
  useEntities,
  useEntitiesPage,
  useKinds,
  type EntityFilter,
  type EntitySort,
  type Member,
  type UseContentEntities,
  type UseEntity,
  type UseEntityMembers,
  type UseEntitiesPage,
  type UseKinds,
} from "./entities.js";
export {
  beaconsFor,
  useBeacons,
  useContentFilters,
  useContents,
  useContentsPage,
  useExposure,
  useHistory,
  useLockedCount,
  useMyFavorites,
  useRecentSearches,
  useRecommendations,
  useReleaseDates,
  useDailyReleases,
  type UseDailyReleases,
  useSearch,
  useSearchPage,
  useSimilar,
  useTypeahead,
  type ExposureOptions,
  type MyInteraction,
  type UseInteractions,
  type ReleaseDate,
  type UseContentFilters,
  type UseContentsPage,
  type UseExposure,
  type UseHistory,
  type UseRail,
  type UseRecentSearches,
  type UseTypeahead,
} from "./discovery.js";
export {
  downloadMessage,
  nextRelease,
  startDownload,
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


export { useReadingEngagement, type ReadingEngagement, type ReadingEngagementOptions } from "./use-engagement.js";
export { usePostReading, usePostReads, type PostReadingOptions, type PostReads } from "./post-reads.js";
export { useRememberedSort } from "./remembered-sort.js";
