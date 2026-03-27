type Status = "healthy" | "warning" | "error" | "unknown";

interface StatusCardProps {
  label: string;
  status: Status;
  detail?: string;
  icon?: string;
}

const STATUS_COLORS: Record<Status, string> = {
  healthy: "var(--success)",
  warning: "var(--warning)",
  error: "var(--danger)",
  unknown: "var(--text-dim)",
};

const STATUS_LABELS: Record<Status, string> = {
  healthy: "Healthy",
  warning: "Warning",
  error: "Error",
  unknown: "Unknown",
};

export function StatusCard({ label, status, detail, icon }: StatusCardProps) {
  const color = STATUS_COLORS[status];
  return (
    <div className="status-card">
      <div className="status-card-header">
        {icon && <span className="status-card-icon">{icon}</span>}
        <span className="status-card-label">{label}</span>
        <span className="status-card-badge" style={{ color, borderColor: color }}>
          {STATUS_LABELS[status]}
        </span>
      </div>
      {detail && <div className="status-card-detail">{detail}</div>}
    </div>
  );
}
