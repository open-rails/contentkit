package signalstore

func init() {
	for name, spec := range measuredSchema {
		expectedSchema[name] = spec
	}
}

var measuredSchema = map[string]tableSpec{
	"engagement_segments": {
		engine: "ReplacingMergeTree", version: "version",
		sortingKey: "tenant, content_kind, content_id, content_version_id, subject_kind, subject, receipt_id", partitionKey: "toYYYYMM(occurred_at)",
		columns: append(append([]columnSpec{{"tenant", "LowCardinality(String)"}}, refColumnSpecs...),
			columnSpec{"subject_kind", "LowCardinality(String)"}, columnSpec{"subject", "String"}, columnSpec{"receipt_id", "String"},
			columnSpec{"revision", "UInt64"}, columnSpec{"visit_id", "String"}, columnSpec{"segment_id", "String"}, columnSpec{"occurred_at", "DateTime64(6, 'UTC')"},
			columnSpec{"active_ms", "UInt32"}, columnSpec{"covered_units", "Array(UInt32)"}, columnSpec{"unit_attention", "Map(UInt32, UInt64)"},
			columnSpec{"unit_total", "UInt32"}, columnSpec{"unit_scale", "UInt32"}, columnSpec{"resume", "String"}, columnSpec{"access_context", "String"},
			columnSpec{"measurement", "String"}, columnSpec{"version", "UInt128"}, columnSpec{"ingested_at", "DateTime64(6, 'UTC')"}, columnSpec{"origin_click_id", "String"}),
	},
	"engagement_state": {
		engine: "ReplacingMergeTree", version: "version", sortingKey: "tenant, subject_kind, subject, content_kind, content_id, content_version_id",
		columns: append(append([]columnSpec{{"tenant", "LowCardinality(String)"}, {"subject_kind", "LowCardinality(String)"}, {"subject", "String"}}, refColumnSpecs...),
			columnSpec{"source_version", "String"}, columnSpec{"first_engaged_at", "DateTime64(6, 'UTC')"}, columnSpec{"last_engaged_at", "DateTime64(6, 'UTC')"},
			columnSpec{"active_ms", "UInt64"}, columnSpec{"covered_units", "Array(UInt32)"}, columnSpec{"unit_attention", "Map(UInt32, UInt64)"},
			columnSpec{"unit_total", "UInt32"}, columnSpec{"unit_scale", "UInt32"}, columnSpec{"qualified", "Bool"}, columnSpec{"coverage", "Float64"},
			columnSpec{"quality", "Float64"}, columnSpec{"completed", "Bool"}, columnSpec{"resume", "String"}, columnSpec{"version", "UInt256"}),
	},
	"engagement_daily": {
		engine: "ReplacingMergeTree", version: "version", sortingKey: "tenant, content_kind, content_id, content_version_id, subject_kind, subject, day", partitionKey: "toYYYYMM(day)",
		columns: append(append([]columnSpec{{"tenant", "LowCardinality(String)"}}, refColumnSpecs...),
			columnSpec{"subject_kind", "LowCardinality(String)"}, columnSpec{"subject", "String"}, columnSpec{"day", "Date"}, columnSpec{"present", "Bool"},
			columnSpec{"active_ms", "UInt64"}, columnSpec{"covered_units", "Array(UInt32)"}, columnSpec{"unit_attention", "Map(UInt32, UInt64)"},
			columnSpec{"unit_total", "UInt32"}, columnSpec{"unit_scale", "UInt32"}, columnSpec{"version", "UInt256"}),
	},
	"legacy_reading_state": {
		engine: "ReplacingMergeTree", version: "version", sortingKey: "tenant, subject_kind, subject, content_kind, content_id, content_version_id",
		columns: append(append([]columnSpec{{"tenant", "LowCardinality(String)"}, {"subject_kind", "LowCardinality(String)"}, {"subject", "String"}}, refColumnSpecs...),
			columnSpec{"first_seen_at", "DateTime('UTC')"}, columnSpec{"last_seen_at", "DateTime('UTC')"}, columnSpec{"progress", "UInt32"}, columnSpec{"progress_max", "UInt32"},
			columnSpec{"completed", "Bool"}, columnSpec{"resume", "String"}, columnSpec{"version", "UInt256"}),
	},
}
