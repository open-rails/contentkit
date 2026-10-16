// Framework-free ContentKit client: one createContentKitClient, its modules,
// one error type, the generated wire types, and the media math the UI uses.
export { createContentKitClient, type ContentKitClient, type ContentKitClientOptions } from "./client.js";
export type { ContentKitChange, ContentKitModule, ContentKitMounts, HlsLoader, Mount, TokenSource } from "./http.js";
export {
  CLIENT_ERROR_CODES,
  ContentKitError,
  ERROR_CODES,
  failureError,
  isContentKitError,
  readContentKitError,
  toContentKitError,
  transportFailure,
  type ContentKitErrorCode,
  type ContentKitErrorInit,
} from "./errors.js";
export { IMAGE_SCHEME, imageRef, imageRefs, ContentsClient, type ManageQuery } from "./contents.js";
export { CatalogClient, type CatalogTotalsQuery } from "./catalog.js";
export { CommentsClient, type AdminCommentQuery, type CommentQuery } from "./content/comments.js";
export { ReactionsClient } from "./content/reactions.js";
export { FavoritesClient, type MineQuery } from "./content/favorites.js";
export { PollsClient, type PollAdminQuery, type PollQuery, type PollState } from "./content/polls.js";
export { BansClient } from "./content/bans.js";
export { ModerationClient, type HeldQuery } from "./content/moderation.js";
export { reactionVerb, sameTarget, type BanScope, type ContentChange, type Decision, type HeldKind, type Reaction, type RuleSet, type Sort, type Target } from "./content/types.js";
export { EntitiesClient, type EntityImageSlot, type EntityQuery, type MemberQuery, type MyEntityQuery } from "./entities.js";
export {
  DiscoveryClient,
  popularQuery,
  type AccessMode,
  type CardQuery,
  type ContentFilterQuery,
  type ContentQuery,
  type HistoryQuery,
  type ListSort,
  type Period,
  type PopularPeriod,
  type PopularQuery,
  type RailQuery,
  type ReaderQuery,
  type RecentSearchesQuery,
  type ReleaseDatesQuery,
  type DailyReleasesQuery,
  type SearchQuery,
  type TypeaheadQuery,
} from "./discovery.js";
export { VersionsClient, isPending } from "./versions.js";
export { emptyReaderMeta, matchAudio, readerMetaProblems, readerNames, type AudioMatch, type ReaderMetaProblem, type ReaderNames } from "./reader-meta.js";
export { OffersClient } from "./offers.js";
export { StatsClient, type ActivityQuery, type TopQuery } from "./stats.js";
export { Beacons, eventId, readerSession, type Attribution, type BeaconOptions, type ViewSession } from "./beacons.js";
export { LETTERS, attributeInput, fold, fromLocalInput, humanize, letterOf, parseAttribute, parseAttributes, slugify, toLocalInput } from "./group-fields.js";
export { CodesClient } from "./codes.js";
export { NotificationsClient, type NotificationItem } from "./notifications.js";
export { ReaderPreferencesClient, type LanguagePreference, type ReaderPreferenceState } from "./reader-preferences.js";
export { BrowserReaderPreferences, type GuestReaderPreferences } from "./browser-reader-preferences.js";
export { call, routeURL, type CallOptions, type Method, type PageQuery, type PathParams, type RouteQuery, type RoutePath } from "./route.js";
export { MediaApi, type ReadOptions } from "./media/api.js";
export {
  MediaClient,
  backoff,
  samePath,
  stem,
  type CommitOptions,
  type CommitSource,
  type ContentFolders,
  type MediaOptions,
  type NamedOptions,
  type NamedUpload,
  type PlannedPart,
  type Progress,
  type PutOptions,
  type UploadOptions,
  type UploadState,
  type UploadedFile,
  type WaitOptions,
} from "./media/client.js";
export { ItemUploadQueue, type ItemStatus, type QueueItem, type QueueOptions, type QueueSnapshot } from "./media/queue.js";
export {
  acceptOf,
  defaultImage,
  stableImage,
  filesFor,
  isPattern,
  ratioLabel,
  ruleDir,
  ruleFor,
  screenFiles,
  uniqueName,
  uploadRules,
  withMediaType,
  type Screened,
} from "./media/rules.js";
export { READ_CHUNK, chunkOffsets, isProcessing, mergeReads, processing } from "./media/windows.js";
export { defaultTransport, fetchTransport, xhrTransport, type Transport } from "./media/transport.js";
export { checkRef, isContentId, itemKey, sameItem } from "./media/ref.js";
export { encodeRemaining } from "./media/encode.js";
export { sha256Hex } from "./media/hash.js";
export { Pacer, type PacerOptions } from "./media/pacer.js";
export { centeredCrop, constrainCrop, editOf, editedSize, fromRotated, rotation, sameEdit, toOriginal, toRotated, type Rotation, type Size } from "./crop.js";
export { decodeImage, isAnimatedImage, type CropSource, type DecodeOptions } from "./image.js";
export { aspectOf, ratio, type AspectRatio } from "./aspect.js";
export { fill, publicRenditions, publicURL, srcSet, type PublicPreset } from "./public.js";
export { DEFAULT_DENSITY, densityFor, pickRendition, sortRenditions, type DensityRange, type Rendition } from "./rendition.js";
export {
  formatDuration,
  galleryItems,
  isAudioType,
  isImageType,
  isVideoType,
  isSubtitleType,
  stageAspect,
  type GalleryItem,
  type GalleryLockedItem,
  type GalleryMediaItem,
  type GalleryView,
} from "./gallery.js";
export {
  ABR_DEFAULTS,
  NETWORK_FAILURES_BEFORE_ERROR,
  abrHlsConfig,
  capRung,
  classifyHlsError,
  classifyMediaError,
  hlsConfig,
  initialEstimate,
  expiryDelay,
  REFRESH_BEFORE_EXPIRY,
  refreshable,
  startRung,
  statusKind,
  type AbrPolicy,
  type ConnectionHint,
  type HlsErrorLike,
  type Rung,
  type PlaybackError,
  type PlaybackErrorKind,
} from "./playback.js";
export {
  PLAYBACK_SPEEDS,
  PLAYER_TRACKS_KEY,
  PLAYER_VOLUME_KEY,
  parseSpriteVtt,
  playerKeyAction,
  resumeAt,
  type PlayerAction,
  type PlayerTrack,
  type SpriteCue,
  type TrackPrefs,
  type VolumePrefs,
} from "./player.js";
export * from "./generated/wire.js";
