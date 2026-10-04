import type { components } from "@/api/schema";

export type DiagnosticReport = components["schemas"]["ReleaseDiagnosticReport"];
export type BuildRecord = components["schemas"]["BuildAcceptance"];
export type Pipeline = components["schemas"]["DeliveryPipelineDetail"];
export type DeliveryRun = components["schemas"]["DeliveryRunDetail"];
export type OperationStatus = components["schemas"]["BuildOperation"]["status"];
