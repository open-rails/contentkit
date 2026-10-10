// Framework-free ContentKit client: one createContentKitClient, its modules,
// one error type, the generated wire types, and the media math the UI uses.
export { createContentKitClient, type ContentKitClient, type ContentKitClientOptions } from "./client.js";
export type { ContentKitChange, ContentKitModule, ContentKitMounts, Mount, TokenSource } from "./http.js";
export {
  CLIENT_ERROR_CODES,
  ContentKitError,
  ERROR_CODES,
  failureError,
  isContentKitError,
  readContentKitError,
  toContentKitError,
  type ContentKitErrorCode,
  type ContentKitErrorInit,
} from "./errors.js";
export { MediaApi, type ReadOptions } from "./media/api.js";
export {
  MediaClient,
  backoff,
  samePath,
  stem,
  type CommitOptions,
  type CommitSource,
  type MediaOptions,
  type PlannedPart,
  type Progress,
  type PutOptions,
  type UploadOptions,
  type UploadState,
  type UploadedFile,
  type WaitOptions,
} from "./media/client.js";
export { UploadQueue, type ItemStatus, type QueueItem, type QueueOptions, type QueueSnapshot } from "./media/queue.js";
export { defaultTransport, fetchTransport, xhrTransport, type Transport } from "./media/transport.js";
export { checkRef, isContentId } from "./media/ref.js";
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
export * from "./generated/wire.js";
