// Types for the Go API's JSON.
//
// Written against responses captured from the running API, not from memory of
// the Go structs -- the read API's flag filter once shipped with two of seven
// names misspelt for exactly that reason. Optional fields are the ones the Go
// side marks omitempty: ABSENT means "not known", which the UI must render
// differently from zero.

export type ScanState = "never_scanned" | "scanning" | "complete";

export interface Library {
  id: string;
  name: string;
  source_kind: string;
  root_path: string;
  last_scan_started_at?: string;
  last_scan_finished_at?: string;
  created_at: string;
  updated_at: string;
  scan_state: ScanState;
  photo_count?: number;
}

export interface MetadataSummary {
  total: number;
  extracted: number;
  pending: number;
  with_exif_date: number;
  with_file_date: number;
  with_no_date: number;
  with_gps: number;
  failed: number;
}

export interface DuplicateSummary {
  groups: number;
  duplicate_files: number;
  reclaimable_bytes: number;
  hashed: number;
  pending_hash: number;
}

export interface SimilarSummary {
  groups: number;
  similar_photos: number;
  reclaimable_bytes: number;
  chained_groups: number;
  phashed: number;
  pending_phash: number;
}

export interface QualitySummary {
  analyzed: number;
  pending: number;
  flagged: number;
  by_flag: Record<string, number>;
  mean_quality_score?: number;
}

export interface ClusterSummary {
  clusters: number;
  clustered_photos: number;
  undated_photos: number;
  located_photos: number;
  low_confidence_clusters: number;
  largest_cluster_photos: number;
}

export interface LibraryDetail {
  library: Library;
  photos_by_state: Record<string, number>;
  metadata: MetadataSummary;
  duplicates: DuplicateSummary;
  similar: SimilarSummary;
  quality: QualitySummary;
  clusters: ClusterSummary;
}

// --- photos -----------------------------------------------------------------

export const QUALITY_FLAGS = [
  "possibly_blurry",
  "possibly_underexposed",
  "possibly_overexposed",
  "possibly_low_contrast",
  "low_resolution",
  "shadows_clipped",
  "highlights_clipped",
] as const;
export type QualityFlag = (typeof QUALITY_FLAGS)[number];

export interface PhotoView {
  id: string;
  library_id: string;
  relative_path: string;
  original_filename: string;
  file_size_bytes: number;
  detected_format: string;
  state: string;
  width?: number;
  height?: number;
  orientation?: number;
  camera_make?: string;
  camera_model?: string;
  captured_at?: string;
  captured_at_source?: "exif" | "filesystem";
  latitude?: number;
  longitude?: number;
  sharpness?: number;
  exposure?: number;
  contrast?: number;
  resolution?: number;
  quality_score?: number;
  quality_flags: string[];
  sha256?: string;
  phash?: string;
  duplicate_group_id?: string;
  similar_group_id?: string;
  cluster_id?: string;
  thumbnail_width?: number;
  thumbnail_height?: number;
  last_error?: string;
}

export type PhotoSort = "path" | "captured_at" | "file_size" | "quality" | "created_at";

export interface PhotoSearchParams {
  q?: string;
  flag?: string;
  has_gps?: boolean;
  has_flags?: boolean;
  has_duplicates?: boolean;
  has_similar?: boolean;
  from?: string;
  to?: string;
  sort?: PhotoSort;
  order?: "asc" | "desc";
  limit?: number;
  offset?: number;
}

export interface PhotoSearchResult {
  photos: PhotoView[];
  total: number;
  limit: number;
  offset: number;
  sort: PhotoSort;
  order: "asc" | "desc";
}

export interface RelatedPhoto {
  photo_id: string;
  relative_path: string;
  file_size_bytes: number;
  width?: number;
  height?: number;
  quality_score?: number;
  distance?: number;
  suggested_keep: boolean;
  is_self: boolean;
}

export interface PhotoDetail {
  photo: PhotoView;
  relations: {
    duplicates: RelatedPhoto[];
    similar: RelatedPhoto[];
    cluster: RelatedPhoto[];
  };
}

// --- groups -----------------------------------------------------------------

export interface DuplicateGroup {
  id: string;
  sha256: string;
  photo_count: number;
  total_bytes: number;
  reclaimable_bytes: number;
  suggested_keep_photo_id?: string;
  created_at: string;
  photos: {
    photo_id: string;
    relative_path: string;
    file_size_bytes: number;
    suggested_keep: boolean;
  }[];
}

export interface DuplicateList {
  groups: DuplicateGroup[];
  summary: DuplicateSummary;
}

export interface SimilarGroup {
  id: string;
  photo_count: number;
  threshold: number;
  max_distance: number;
  reclaimable_bytes: number;
  suggested_keep_photo_id?: string;
  chained: boolean;
  photos: {
    photo_id: string;
    relative_path: string;
    file_size_bytes: number;
    width?: number;
    height?: number;
    quality_score?: number;
    distance: number;
    rank: number;
    suggested_keep: boolean;
  }[];
}

export interface SimilarList {
  groups: SimilarGroup[];
  summary: SimilarSummary;
}

export interface Cluster {
  id: string;
  photo_count: number;
  started_at: string;
  ended_at: string;
  anchor_latitude?: number;
  anchor_longitude?: number;
  max_distance_meters: number;
  located_count: number;
  filesystem_dated_count: number;
  confidence: "high" | "mixed" | "low";
  photos: {
    photo_id: string;
    relative_path: string;
    sequence: number;
    captured_at?: string;
    distance_meters?: number;
    latitude?: number;
    longitude?: number;
  }[];
}

export interface ClusterList {
  clusters: Cluster[];
  summary: ClusterSummary;
}

// --- queue ------------------------------------------------------------------

export interface JobCounts {
  pending: number;
  running: number;
  succeeded: number;
  dead: number;
  total: number;
}

export interface JobsResponse {
  by_type: Record<string, JobCounts>;
  total: JobCounts;
}

export interface ApiErrorBody {
  error: { code: string; message: string };
}
