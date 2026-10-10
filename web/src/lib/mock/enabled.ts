// Mock switch. Public variables injected at build time (only the NEXT_PUBLIC_ prefix is browser-readable).
// Set NEXT_PUBLIC_MOCK=1 on Vercel and the whole site runs on mocks with no backend needed.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
