// 表单只预检长度；弱口令和完整策略始终由服务端裁决。
export function temporaryPasswordValid(value: string) {
  const characters = Array.from(value);
  return (
    characters.length >= 12 &&
    characters.length <= 128 &&
    new TextEncoder().encode(value).length <= 512
  );
}
