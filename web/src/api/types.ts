export type LogEventType = 'info' | 'warn' | 'warning' | 'error' | 'config' | 'success';

export interface TaskStats {
    total: number;
    printed: number;
    buffered: number;
}

export interface LineTask {
    task_id: number;
    line_id: number;
    line_name?: string;
    template_name: string;
    dynamic_field_name?: string;
    status: string;
    created_at?: string;
    rnd_text?: string;
    stats?: TaskStats;
    printed?: number;
    total?: number;
    eta?: string;
}

export interface LivePrinterItem {
    id: number;
    name: string;
    ip: string;
    port: number;
    driver_type: string;
    is_active: boolean;
    status: string;
    ribbon: string | null;
    queue_free: number;
    cur_count: string;
    cur_template: string;
    role?: string;
    buffer_limit?: number;
    lead_loop?: number;
}

export interface ScannerItem {
    id: number;
    line_id: number;
    target_device_id: number | null;
    name: string;
    address: string;
    port: number;
    role: string;
    is_active: boolean;
    scans_count?: number;
    rejects_count?: number;
}

export interface LiveLineItem {
    line_id: number;
    line_name: string;
    description: string;
    is_active: boolean;
    status: string;
    current_task: LineTask | null;
    printers: number[];
}

export interface DashboardLiveResponse {
    timestamp: string;
    summary: {
        total_lines: number;
        active_tasks: number;
        idle_lines: number;
        printers_offline: number;
        total_printers: number;
    };
    lines: LiveLineItem[];
    printers: Record<string, LivePrinterItem>;
}

export interface PrintersResponse {
    lines: Array<{
        id: number;
        name: string;
        description: string;
        is_active: boolean;
        printers: Array<{
            id: number;
            name: string;
            ip: string;
            port: number;
            driver_type: string;
            is_active: boolean;
        }>;
        scanners: ScannerItem[];
    }>;
    all_printers: LivePrinterItem[];
}

export interface EventLogItem {
    id: number;
    timestamp: string;
    line_id: number | null;
    line_name: string;
    printer_id: number | null;
    printer_name: string;
    event_type: LogEventType;
    message: string;
}

export interface LogsHistoryResponse {
    status: string;
    count: number;
    items: EventLogItem[];
}

export interface TaskStopResponse {
    task_id: number;
    line_id: number;
    status: 'stopped';
    timestamp: string;
    total_confirmed: number;
    remaining_pending: number;
    buffer_reverted_total: number;
    rnd_text?: string;
    warnings?: string[];
}