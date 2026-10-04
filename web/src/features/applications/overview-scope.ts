import type { Query, QueryClient } from "@tanstack/react-query";
import { buildQueryKeys } from "./builds/api";
import { pipelineKeys } from "./delivery/pipeline-api";
import { releaseQueryKeys } from "./releases/api";

/** 概览不是资源身份。通过已加载的关联记录限定刷新范围，避免刷新其他应用。 */
export function overviewScope(client: QueryClient, applicationId: string) {
  const targetIds = new Set<string>();
  const releaseIds = new Set<string>();
  const pipelineIds = new Set<string>();
  const targets = client.getQueryData<{ id: string }[]>([
    "deployment-targets",
    applicationId,
  ]);
  targets?.forEach((target) => targetIds.add(target.id));
  for (const [, page] of client.getQueriesData<{
    items: { pipeline: { id: string } }[];
  }>({ queryKey: pipelineKeys.lists(applicationId) })) {
    page?.items.forEach((item) => pipelineIds.add(item.pipeline.id));
  }
  for (const id of targetIds) {
    for (const [, data] of client.getQueriesData<
      | { release: { id: string } }
      | { items: { release: { id: string } }[] }
      | null
    >({ queryKey: releaseQueryKeys.lists(id) })) {
      if (!data) continue;
      if ("items" in data)
        data.items.forEach((item) => releaseIds.add(item.release.id));
      else releaseIds.add(data.release.id);
    }
  }
  return (query: Query) => {
    const [kind, id] = query.queryKey;
    if (typeof id !== "string") return false;
    if (
      kind === buildQueryKeys.lists(applicationId)[0] ||
      kind === pipelineKeys.lists(applicationId)[0] ||
      kind === "deployment-targets"
    )
      return id === applicationId;
    if (kind === "target-releases") return targetIds.has(id);
    if (kind === "release-diagnostics") return releaseIds.has(id);
    if (kind === "delivery-pipeline") return pipelineIds.has(id);
    return false;
  };
}
