// Package e2e runs ContentKit end to end over PostgreSQL, MinIO and a real
// AuthKit (authtest) passed straight in as its auth.Authenticator and
// userinfo.Lookup. It is a module of its own: the core imports AuthKit
// contracts, while this suite also links the AuthKit runtime. It needs CONTENTKIT_TEST_URL, CONTENTKIT_TEST_S3_* and
// AUTHKIT_TEST_DATABASE_URL.
package e2e
