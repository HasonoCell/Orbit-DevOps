/** 导航标识不重复声明旁边已有的产品名称。 */
export function OrbitMark() {
  return <svg viewBox="0 0 32 32" className="size-8 shrink-0 text-primary" fill="none" aria-hidden="true">
    <circle cx="16" cy="16" r="8" stroke="currentColor" strokeWidth="2" />
    <ellipse cx="16" cy="16" rx="15" ry="6" transform="rotate(-40 16 16)" stroke="currentColor" strokeWidth="2" />
    <circle cx="25" cy="8" r="3" fill="currentColor" />
  </svg>;
}
