-- parent: 5 sha256:a97b38f508005b8245db2ab8f6a715a57885e3feaf7e714ad8265b851dd82beb
-- Watch history is ordered and cleared by consumption activity, not by later
-- clicks or feedback on the same content.

ALTER TABLE subject_content_state {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS last_view_at DateTime('UTC') DEFAULT toDateTime(0, 'UTC') AFTER last_signal_at;
