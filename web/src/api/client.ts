import {
    DashboardLiveResponse,
    LogsHistoryResponse,
    PrintersResponse,
    TaskStopResponse,
} from './types';

export const api = {
    getDashboardLive: async (): Promise<DashboardLiveResponse> => {
        const res = await fetch('/api/dashboard/live');
        if (!res.ok) throw new Error(`HTTP error ${res.status}`);
        return res.json();
    },

    getPrintersTopology: async (): Promise<PrintersResponse> => {
        const res = await fetch('/api/printers');
        if (!res.ok) throw new Error(`HTTP error ${res.status}`);
        return res.json();
    },

    getLogsHistory: async (limit = 50): Promise<LogsHistoryResponse> => {
        const res = await fetch(`/api/logs/history?limit=${limit}`);
        if (!res.ok) throw new Error(`HTTP error ${res.status}`);
        return res.json();
    },

    stopTask: async (taskId: number): Promise<TaskStopResponse> => {
        const res = await fetch(`/api/task/stop?task_id=${taskId}`, {
            method: 'POST',
        });
        if (!res.ok) {
            const err = await res.text();
            throw new Error(err || `Failed to stop task #${taskId}`);
        }
        return res.json();
    },
};