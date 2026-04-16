export { Client, LeaseLostError } from "./client.js";
export { DEFAULT_BASE_URL, DEFAULT_NAMESPACE, type ClientOptions } from "./client.js";

export { Worker } from "./worker.js";
export type { WorkerConfig, ActionFn, Logger } from "./worker.js";

export type {
  JobClaim,
  JobClaimDataResponse,
  JobClaimRequest,
  JobCompleteRequest,
  JobFenceRequest,
  JobHeartbeat,
  JobHeartbeatDataResponse,
  JobHeartbeatDirectives,
} from "./api/index.js";
