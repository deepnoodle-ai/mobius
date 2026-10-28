/**
 * The SDK's version. Keep it equal to `version` in package.json; a test
 * (test/client-header.test.ts) fails when the two drift.
 */
export const SDK_VERSION = "0.5.1";

/**
 * Names the program that made a request, for the server's usage logs. It is
 * reporting only: the server authorizes nothing on it.
 */
export const MOBIUS_CLIENT_HEADER = "X-Mobius-Client";

/** The X-Mobius-Client value every request sends: `sdk-ts/<version>`. */
export const MOBIUS_CLIENT = `sdk-ts/${SDK_VERSION}`;
