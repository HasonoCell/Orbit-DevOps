import { expect, type Locator } from "@playwright/test";

/** 从可见菜单选择业务值，覆盖真实弹层交互而不是绕过组件写隐藏字段。 */
export async function chooseOption(
  trigger: Locator,
  value: string | { index: number },
) {
  await trigger.click();
  const menu = trigger.page().getByRole("listbox");
  await expect(menu).toBeVisible();
  const option =
    typeof value === "string"
      ? menu.locator(
          `[data-slot="select-item"][data-value=${JSON.stringify(value)}]`,
        )
      : menu.getByRole("option").nth(value.index);
  await option.click();
  await expect(menu).toHaveCount(0);
}
