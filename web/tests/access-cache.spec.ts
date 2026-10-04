import { expect, test } from "@playwright/test";
import { QueryClient } from "@tanstack/react-query";
import { invalidateHostTargetRoutes } from "../src/features/projects/access/target-route-cache";

test("Host 变化刷新相关 Target 的所有分页，保留其他项目缓存", () => {
  const client = new QueryClient();
  const first = ["target-access-routes", "target-1", 21, 0];
  const second = ["target-access-routes", "target-1", 21, 20];
  const related = ["target-access-routes", "target-2", 21, 20];
  const unrelated = ["target-access-routes", "other-target", 21, 0];
  client.setQueryData(first, [{ hostId: "host-1" }]);
  client.setQueryData(second, [{ hostId: "host-2" }]);
  client.setQueryData(related, []);
  client.setQueryData(unrelated, [{ hostId: "other-host" }]);
  client.setQueryData(
    ["access-routes", "project-1", "host-1", 21, 20],
    [{ deploymentTargetId: "target-2" }],
  );
  invalidateHostTargetRoutes(client, "project-1", "host-1");
  for (const key of [first, second, related])
    expect(client.getQueryState(key)?.isInvalidated).toBe(true);
  expect(client.getQueryState(unrelated)?.isInvalidated).toBe(false);
  client.clear();
});
