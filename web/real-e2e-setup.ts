import { request, type FullConfig } from "@playwright/test";
import { assertRealBackend } from "./tests/helpers/real-environment";

export default async function setup(config: FullConfig) {
  const client = await request.newContext({
    baseURL: config.projects[0].use.baseURL,
  });
  try {
    await assertRealBackend(client);
  } finally {
    await client.dispose();
  }
}
