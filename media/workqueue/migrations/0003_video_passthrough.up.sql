-- parent: 2 sha256:47be2bd174f83e8190e68edb819dac96e0a4db23e337f052157d1619f3e8ecab
ALTER TABLE encode_run ADD COLUMN passthrough_codec text NOT NULL DEFAULT ''
    CHECK (passthrough_codec IN ('', 'h264', 'hevc'));
