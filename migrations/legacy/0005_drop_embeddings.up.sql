-- parent: 4 sha256:006211a95cd685cb4acfb39000b02656d1debcde2ee36ab2e9fbf6a56774ffa3
-- The embedding plane left ContentKit; User Intelligence owns its own index.
-- Export these tables first (docs/migration.md); they are rebuildable.
DROP TABLE embedding_dead_letters;
DROP TABLE embedding_vectors_backfill_state;
DROP TABLE embedding_tasks;
DROP TABLE embedding_vectors;
DROP TABLE embedding_models;
