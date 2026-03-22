import { useState, useEffect, useCallback } from "preact/hooks";
import { get, api } from "../api";

export interface ServiceInfo {
  name: string;
  port: number;
  status: "healthy" | "degraded" | "offline" | "disabled" | "starting";
  pid?: number;
}

export function useServices() {
  const [services, setServices] = useState<ServiceInfo[]>([]);
  const [startupError, setStartupError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const svc = await get<ServiceInfo[]>("/api/admin/services");
      setServices(svc);
      setStartupError(null);
    } catch {
      setServices([{ name: "daemon", port: 11435, status: "offline" }]);
      setStartupError("Daemon is offline");
    }
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, 10_000); // poll every 10s
    return () => clearInterval(id);
  }, [refresh]);

  const restart = async (name: string) => {
    await api(`/api/admin/services/${name}/restart`, { method: "POST" });
    // The daemon will restart, poll will pick up the status
  };

  const stop = async (name: string) => {
    await api(`/api/admin/services/${name}/stop`, { method: "POST" });
  };

  return { services, restart, stop, startupError, refresh };
}
