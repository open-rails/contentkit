-- A person's day holds opens, searches and follows, not only reading: the
-- history is the interaction history.
--
-- One row per person and UTC day: the day's actions in time order, packed.
-- A version byte (2), then per action: a header byte (kind in bits 0-1:
-- 0 gallery, 1 search, 2 follow; bit 2 marks a session start), the UTC offset
-- in 15-minute steps (int8), the uvarint minute of the UTC day, then for a
-- gallery its 16-byte id, a byte (fraction read in tenths, bits 0-3; the
-- reaction, bits 4-6: 0 none, 1 like, 2 dislike, 3 favorite, 4 hidden), an
-- active-time byte (0 none, n for [2^(n-1), 2^n) seconds) and the surface of
-- the day's first open (readingstore.Surface: 0 outside, 1 home, 2 list,
-- 3 popular, 4 similar, 5 search, 6 entity, 7 recommended, 8 continue or
-- history, 9 favorites, 15 other); for a search a uvarint length and the
-- UTF-8 query; for a follow the 16-byte entity id.
ALTER TABLE reading_history RENAME TO interaction_history;
ALTER TABLE interaction_history RENAME CONSTRAINT reading_history_actions_check TO interaction_history_actions_check;
ALTER TABLE interaction_history RENAME CONSTRAINT reading_history_pkey TO interaction_history_pkey;
