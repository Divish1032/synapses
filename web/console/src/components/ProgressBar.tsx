interface ProgressBarProps {
  value: number;   // 0–100
  label?: string;
  color?: string;
  showPercent?: boolean;
}

export function ProgressBar({ value, label, color, showPercent = true }: ProgressBarProps) {
  const pct = Math.min(100, Math.max(0, value));
  return (
    <div className="progress-bar-container">
      {(label || showPercent) && (
        <div className="progress-bar-header">
          {label && <span className="progress-bar-label">{label}</span>}
          {showPercent && <span className="progress-bar-pct">{Math.round(pct)}%</span>}
        </div>
      )}
      <div className="progress-bar-track">
        <div
          className="progress-bar-fill"
          style={{
            width: `${pct}%`,
            background: color ?? "var(--accent)",
          }}
        />
      </div>
    </div>
  );
}
