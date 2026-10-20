// describeCron renders the common five-field shapes in Chinese. Anything more
// exotic keeps the raw expression rather than guessing.
export function describeCron(expr: string): string {
  const parts = expr.trim().split(/\s+/)
  if (parts.length !== 5) return expr
  const [minute, hour, day, month, weekday] = parts
  if (day !== '*' || month !== '*' || weekday !== '*') return expr
  const two = (value: number) => String(value).padStart(2, '0')
  if (minute.startsWith('*/') && hour === '*') return `每 ${minute.slice(2)} 分钟`
  if (hour.startsWith('*/') && /^\d+$/.test(minute)) return `每 ${hour.slice(2)} 小时的第 ${minute} 分`
  if (/^\d+$/.test(minute) && /^\d+$/.test(hour)) return `每天 ${two(Number(hour))}:${two(Number(minute))}`
  return expr
}
