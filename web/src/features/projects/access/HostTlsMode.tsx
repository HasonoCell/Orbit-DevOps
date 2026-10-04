import { Label } from "@/components/ui/label";
import { Select, SelectItem } from "@/components/ui/select";
import type { Host, HostInput } from "./types";

/** 创建和编辑复用模式选择，是否允许托管证书由当前服务端 Policy 列表裁决。 */
export function HostTlsMode({
  id,
  label,
  value,
  managedAvailable,
  onChange,
}: {
  id: string;
  label: string;
  value: Host["tlsMode"];
  managedAvailable: boolean;
  onChange: (mode: Host["tlsMode"]) => void;
}) {
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <Select
        id={id}
        className="w-full"
        value={value}
        onValueChange={(mode) => {
          if (
            mode === "http_only" ||
            mode === "managed" ||
            mode === "existing_secret"
          )
            onChange(mode);
        }}
      >
        <SelectItem value="http_only">仅 HTTP</SelectItem>
        <SelectItem value="managed" disabled={!managedAvailable}>
          托管证书
        </SelectItem>
        <SelectItem value="existing_secret">已有 TLS Secret</SelectItem>
      </Select>
    </div>
  );
}

/** 不同模式只发送对应字段，避免历史草稿中的 Policy/Binding 泄漏到另一种命令。 */
export function hostInput(
  hostname: string,
  tlsMode: Host["tlsMode"],
  policyKey: string,
  bindingId: string,
): HostInput {
  return {
    hostname,
    tlsMode,
    ...(tlsMode === "managed" ? { issuerPolicyKey: policyKey } : {}),
    ...(tlsMode === "existing_secret" ? { secretBindingId: bindingId } : {}),
  };
}
