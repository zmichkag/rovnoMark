import React, { useState, useEffect, useCallback, useRef } from 'react';

// ============================================================================
// 1. ТИПЫ ДАННЫХ И КОНТРАКТЫ API ROVNOMARK
// ============================================================================

export type DeviceRole = 'first' | 'second' | 'summary' | 'PRIMARY' | 'SECONDARY' | 'SUMMARY';

export interface TaskStats {
    printed: number;
    total: number;
}

export interface CurrentTaskDTO {
    task_id: number;
    template_name: string;
    printed: number;
    scanned: number;
    total: number;
    eta?: string;
    stats?: TaskStats;
}

export interface PrinterDTO {
    id: number;
    line_id: number | null;
    name: string;
    ip: string;
    port: number;
    driver: string;
    status: string;
    is_active: boolean;
    ribbon: number;
    queue: number;
    printer_role: DeviceRole;
    buffer_size: number;
    lead_loop: number;
    pumper_mode: string;
    printed: number;
    target: number;
}

export interface LastScannedCodeInfo {
    code: string;
    timestamp: string;
    match_status: 'matched' | 'unmatched' | 'no_read';
    scanner_name?: string;
    weight?: string;
}

export interface ScannerDTO {
    id: number;
    line_id: number;
    bound_printer_id: number | null; // target_device_id
    name: string;
    address: string;
    port: number;
    role: string;
    is_active: boolean;
    scans: number;
    rejects: number;
    status: string;
    last_scan?: LastScannedCodeInfo;
}

export interface ScannerReadItem {
    id: number;
    scanner_name: string;
    code: string;
    match_status: 'matched' | 'unmatched' | 'no_read' | string;
    read_at: string;
    weight?: string;
}

export interface LineDTO {
    id: number;
    name: string;
    description: string;
    mode: string;
    status: 'active' | 'ready' | 'idle' | 'error';
    is_active: boolean;
    current_task: CurrentTaskDTO | null;
    printerIds: number[];
    scannerIds?: number[];
    config_type?: 'dual_head' | 'catchweight' | 'aggregation' | 'manual' | 'failure';
    weight_sample?: string;
    aggregation_ratio?: string;
    last_scan?: LastScannedCodeInfo;
}

export interface SummaryDTO {
    active_tasks: number;
    total_lines: number;
    printers_offline: number;
}

export interface LogItemDTO {
    id?: number;
    time: string;
    printer: string;
    line?: string;
    message: string;
    type: 'info' | 'warn' | 'error' | 'config' | 'success';
}

export interface StopTaskResponse {
    task_id: number;
    line_id: number;
    status: string;
    timestamp: string;
    total_confirmed: number;
    remaining_pending: number;
    buffer_reverted_total: number;
    warnings?: string[];
}

// ============================================================================
// 2. ВСТРОЕННЫЕ SVG-ИКОНКИ (ZERO-DEPENDENCY)
// ============================================================================

const Icons = {
    Activity: () => (
        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M13 10V3L4 14h7v7l9-11h-7z" />
        </svg>
    ),
    Barcode: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M12 4v16m-4-16v16m-4-12v8m16-8v8m-4-12v16m4-12v8" />
        </svg>
    ),
    Copy: () => (
        <svg className="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" />
        </svg>
    ),
    Check: () => (
        <svg className="w-3 h-3 text-emerald-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M5 13l4 4L19 7" />
        </svg>
    ),
    List: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 6h16M4 10h16M4 14h16M4 18h16" />
        </svg>
    ),
    Sparkles: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M5 3v4M3 5h4M6 17v4m-2-2h4m5-16l2.286 6.857L21 12l-5.714 2.286L13 21l-2.286-6.857L5 12l5.714-2.286L13 3z" />
        </svg>
    ),
    Printer: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M6 9V2h12v7M6 18H4a2 2 0 01-2-2v-5a2 2 0 012-2h16a2 2 0 012 2v5a2 2 0 01-2 2h-2m-12 0v4h12v-4" />
        </svg>
    ),
    Camera: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z" />
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M15 13a3 3 0 11-6 0 3 3 0 016 0z" />
        </svg>
    ),
    Stop: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <rect x="6" y="6" width="12" height="12" rx="2" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
    ),
    Shield: () => (
        <svg className="w-5 h-5 text-rose-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z" />
        </svg>
    ),
    Clock: () => (
        <svg className="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <circle cx="12" cy="12" r="10" strokeWidth="2" />
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M12 6v6l4 2" />
        </svg>
    ),
    Terminal: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M8 9l3 3-3 3m5 0h3M5 20h14a2 2 0 002-2V6a2 2 0 00-2-2H5a2 2 0 00-2 2v12a2 2 0 002 2z" />
        </svg>
    ),
    Refresh: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
        </svg>
    ),
    Link: () => (
        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M13.828 10.172a4 4 0 00-5.656 0l-4 4a4 4 0 105.656 5.656l1.102-1.101m-.758-4.899a4 4 0 005.656 0l4-4a4 4 0 00-5.656-5.656l-1.1 1.1" />
        </svg>
    ),
    Chart: () => (
        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
        </svg>
    ),
    Layers: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
        </svg>
    ),
    Maximize: () => (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5l-5-5m5 5v-4m0 4h-4" />
        </svg>
    )
};

// ============================================================================
// 3. UI-ХЕЛПЕРЫ И БЕЙДЖИ
// ============================================================================

const ConfigTypeBadge: React.FC<{ type?: string; mode?: string }> = ({ type, mode }) => {
    if (type === 'dual_head') {
        return (
            <span className="px-2 py-0.5 rounded text-[10px] font-mono font-bold bg-indigo-50 text-indigo-700 border border-indigo-200">
        Двухголовочная (Чет/Нечет)
      </span>
        );
    }
    if (type === 'catchweight') {
        return (
            <span className="px-2 py-0.5 rounded text-[10px] font-mono font-bold bg-amber-50 text-amber-800 border border-amber-200">
        Catchweight (Динамический вес)
      </span>
        );
    }
    if (type === 'aggregation') {
        return (
            <span className="px-2 py-0.5 rounded text-[10px] font-mono font-bold bg-purple-50 text-purple-700 border border-purple-200">
        Групповая агрегация (Короба)
      </span>
        );
    }
    if (type === 'manual') {
        return (
            <span className="px-2 py-0.5 rounded text-[10px] font-mono font-bold bg-teal-50 text-teal-800 border border-teal-200">
        Ручная проклейка (Lead Loop)
      </span>
        );
    }
    return <span className="text-[11px] text-slate-400 font-medium">{mode || 'Прямой поток'}</span>;
};

const RoleBadge: React.FC<{ role: string }> = ({ role }) => {
    const r = (role || 'first').toLowerCase();
    if (r.includes('first') || r.includes('primary') || r.includes('lane_1')) {
        return <span className="px-1.5 py-0.2 rounded text-[9px] font-bold bg-blue-100 text-blue-800 border border-blue-200">1-й (Первый)</span>;
    }
    if (r.includes('second') || r.includes('secondary') || r.includes('even')) {
        return <span className="px-1.5 py-0.2 rounded text-[9px] font-bold bg-indigo-100 text-indigo-800 border border-indigo-200">2-й (Второй)</span>;
    }
    return <span className="px-1.5 py-0.2 rounded text-[9px] font-bold bg-purple-100 text-purple-800 border border-purple-200">Σ (Суммарный)</span>;
};

const ActiveBadge: React.FC<{ isActive: boolean }> = ({ isActive }) => {
    if (isActive) {
        return <span className="px-2 py-0.5 rounded text-[10px] font-bold bg-emerald-100 text-emerald-800 border border-emerald-200">● Активен</span>;
    }
    return <span className="px-2 py-0.5 rounded text-[10px] font-bold bg-slate-200 text-slate-600 border border-slate-300">○ Резерв</span>;
};

const LineStatusBadge: React.FC<{ status: string; isActive: boolean }> = ({ status, isActive }) => {
    if (!isActive) {
        return <span className="px-2.5 py-1 rounded-md text-[10px] font-bold bg-slate-200 text-slate-700">ЛИНИЯ НЕАКТИВНА</span>;
    }
    if (status === 'active') {
        return (
            <span className="px-2.5 py-1 rounded-md text-[10px] font-bold bg-emerald-100 text-emerald-700 border border-emerald-200 flex items-center gap-1.5">
        <span className="relative flex h-2 w-2">
          <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
          <span className="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
        </span>
        В РАБОТЕ
      </span>
        );
    }
    if (status === 'error') {
        return (
            <span className="px-2.5 py-1 rounded-md text-[10px] font-bold bg-rose-100 text-rose-700 border border-rose-200 flex items-center gap-1.5">
        <span className="w-2 h-2 rounded-full bg-rose-500 animate-ping"></span>
        АВАРИЯ
      </span>
        );
    }
    return <span className="px-2.5 py-1 rounded-md text-[10px] font-bold bg-blue-100 text-blue-700">ГОТОВА</span>;
};

// ============================================================================
// 4. КОМПОНЕНТ СВЯЗКИ: «ПРИНТЕР ⇄ КАМЕРА» СО СКАНЕРОМ И ПОСЛЕДНИМ СКАНОМ
// ============================================================================

interface DevicePairCardProps {
    printer: PrinterDTO;
    scanner?: ScannerDTO;
    lineTarget: number;
    onOpenScanHistory?: () => void;
    onCopyCode?: (code: string, id: string) => void;
    copiedCodeId?: string | null;
}

const DevicePairCard: React.FC<DevicePairCardProps> = ({
                                                           printer,
                                                           scanner,
                                                           lineTarget,
                                                           onOpenScanHistory,
                                                           onCopyCode,
                                                           copiedCodeId,
                                                       }) => {
    const isOff = !printer.is_active || printer.status.includes('ОФФЛАЙН') || printer.status === 'НЕАКТИВЕН' || printer.status === 'ERROR';
    const pPrinted = printer.printed || 0;
    const pTarget = printer.target > 0 ? printer.target : lineTarget > 0 ? lineTarget : 5000;
    const pPct = pTarget > 0 ? Math.min(Math.round((pPrinted / pTarget) * 100), 100) : 0;

    const ribbonCritical = printer.ribbon <= 15;
    const ribbonWarning = printer.ribbon <= 30 && printer.ribbon > 15;

    return (
        <div className={`p-3.5 rounded-xl border flex flex-col gap-2.5 transition-all ${
            isOff ? 'bg-rose-50/20 border-rose-200 opacity-90' : 'bg-white border-slate-200 shadow-sm'
        }`}>
            {/* Шапка принтера */}
            <div className="flex justify-between items-start">
                <div>
                    <div className="text-xs font-bold text-slate-900 flex items-center gap-1.5 flex-wrap">
                        <Icons.Printer />
                        <span className="truncate max-w-[140px] sm:max-w-[180px]">{printer.name}</span>
                        <span className="text-[9px] px-1.5 py-0.2 bg-slate-200 text-slate-700 font-mono font-bold rounded uppercase">
              {printer.driver}
            </span>
                        {printer.lead_loop > 0 && (
                            <span className="text-[9px] px-1.5 py-0.2 bg-amber-100 text-amber-800 font-mono font-bold rounded">
                Петля: {printer.lead_loop}
              </span>
                        )}
                    </div>
                    <div className="text-[10px] text-slate-400 font-mono mt-0.5">
                        {printer.ip}:{printer.port} • Буфер: {printer.queue}/{printer.buffer_size}
                    </div>
                </div>
                <div className="flex flex-col items-end gap-1">
                    <ActiveBadge isActive={printer.is_active} />
                    <RoleBadge role={printer.printer_role} />
                </div>
            </div>

            {/* Риббон и аппаратный статус */}
            <div className="flex justify-between items-center text-[10px] pt-1 border-t border-slate-100">
        <span className="text-slate-500 font-medium">
          Статус: <b className={isOff ? 'text-rose-600' : 'text-emerald-700'}>{printer.status}</b>
        </span>
                <div className="flex items-center gap-1.5">
                    <span className="text-slate-400">Риббон:</span>
                    <span className={`font-mono font-bold ${ribbonCritical ? 'text-rose-600 animate-pulse' : ribbonWarning ? 'text-amber-500' : 'text-slate-700'}`}>
            {printer.ribbon > 0 ? `${printer.ribbon}%` : 'N/A'}
          </span>
                </div>
            </div>

            {/* ПРОГРЕСС-БАР #1: ВЫРАБОТКА МАРКИРАТОРА */}
            <div className="p-2 bg-slate-50 rounded-lg border border-slate-200 flex flex-col gap-1">
                <div className="flex justify-between text-[10px] font-mono">
          <span className="text-slate-500 font-bold flex items-center gap-1">
            <span className="text-emerald-600">●</span> Выработка принтера:
          </span>
                    <span className={`font-bold ${printer.is_active && !isOff ? 'text-emerald-700' : 'text-slate-400'}`}>
            {pPrinted.toLocaleString()} / {pTarget.toLocaleString()} ({pPct}%)
          </span>
                </div>
                <div className="w-full h-1.5 bg-slate-200 rounded-full overflow-hidden">
                    <div
                        className={`h-full transition-all duration-700 ${printer.is_active && !isOff ? 'bg-emerald-500' : 'bg-slate-400'}`}
                        style={{ width: `${pPct}%` }}
                    />
                </div>
            </div>

            {/* ПЛАШКА СКАНЕРА: ПРОГРЕСС-БАР #2 И ПОСЛЕДНИЙ ОТСКАНТРОВАННЫЙ КОД */}
            {scanner ? (
                <div className={`p-2.5 rounded-lg text-xs flex flex-col gap-2 ${
                    scanner.is_active ? 'bg-purple-50/70 border border-purple-200' : 'bg-slate-100 border border-slate-200'
                }`}>
                    <div className="flex justify-between items-center">
            <span className="font-bold text-purple-900 flex items-center gap-1.5 truncate max-w-[180px]">
              <Icons.Camera />
                {scanner.name}
            </span>
                        <ActiveBadge isActive={scanner.is_active} />
                    </div>

                    {scanner.is_active ? (
                        <div className="p-2 bg-white rounded-lg border border-purple-100 flex flex-col gap-2">
                            {(() => {
                                const sScans = scanner.scans || 0;
                                const sRejects = scanner.rejects || 0;
                                const matchPct = pPrinted > 0 ? Math.min(Math.round((sScans / pPrinted) * 100), 100) : 100;
                                const sPct = pTarget > 0 ? Math.min(Math.round((sScans / pTarget) * 100), 100) : 0;
                                const lastScan = scanner.last_scan;

                                return (
                                    <>
                                        {/* Прогресс-бар сканера */}
                                        <div className="flex flex-col gap-1">
                                            <div className="flex justify-between text-[10px] font-mono">
                        <span className="text-purple-700 font-bold flex items-center gap-1">
                          <span className="text-purple-600">●</span> Верификация камеры:
                        </span>
                                                <span className="font-bold text-purple-900">
                          {sScans.toLocaleString()} / {pTarget.toLocaleString()} ({sPct}%)
                        </span>
                                            </div>
                                            <div className="w-full h-1.5 bg-purple-100 rounded-full overflow-hidden">
                                                <div
                                                    className="h-full bg-purple-600 transition-all duration-700"
                                                    style={{ width: `${sPct}%` }}
                                                />
                                            </div>
                                            <div className="grid grid-cols-3 gap-1 font-mono text-[10px] text-slate-600 pt-1 border-t border-purple-50">
                                                <div>Скан: <b className="text-purple-900">{sScans.toLocaleString()}</b></div>
                                                <div>Сверка: <b className={matchPct < 99 ? 'text-amber-600 font-bold' : 'text-emerald-700'}>{matchPct}%</b></div>
                                                <div>Брак: <b className={sRejects > 0 ? 'text-rose-600 font-bold' : 'text-slate-500'}>{sRejects}</b></div>
                                            </div>
                                        </div>

                                        {/* БЛОК ПОСЛЕДНЕГО СКАНИРОВАНИЯ ВНУТРИ ПЛАШКИ СКАНЕРА */}
                                        <div className="mt-1 pt-2 border-t border-purple-100/90 flex flex-col gap-1.5 bg-purple-50/50 p-2 rounded-lg border border-purple-100">
                                            <div className="flex items-center justify-between gap-1 text-[9px] font-mono">
                        <span className="text-purple-800 font-bold flex items-center gap-1">
                          <Icons.Barcode /> ПОСЛЕДНИЙ СКАН:
                        </span>
                                                <div className="flex items-center gap-1">
                                                    {lastScan?.timestamp && (
                                                        <span className="text-slate-500 font-semibold">{lastScan.timestamp}</span>
                                                    )}
                                                    {lastScan?.weight && (
                                                        <span className="text-emerald-800 bg-emerald-100 px-1 rounded font-bold">
                              {lastScan.weight}
                            </span>
                                                    )}
                                                </div>
                                            </div>

                                            <div className="flex items-center justify-between gap-1.5">
                                                <div className="flex items-center gap-1 min-w-0 flex-1">
                          <span
                              className={`font-mono text-[10px] truncate select-all ${
                                  lastScan?.match_status === 'no_read'
                                      ? 'text-rose-600 font-bold'
                                      : 'text-slate-900 font-semibold'
                              }`}
                              title={lastScan?.code || 'Ожидание первого считывания...'}
                          >
                            {lastScan?.code || 'Код не зарегистрирован'}
                          </span>
                                                    {lastScan?.code && onCopyCode && (
                                                        <button
                                                            type="button"
                                                            onClick={() => onCopyCode(lastScan.code, `scan_${scanner.id}`)}
                                                            className="text-purple-400 hover:text-purple-700 p-0.5 shrink-0"
                                                            title="Скопировать код"
                                                        >
                                                            {copiedCodeId === `scan_${scanner.id}` ? <Icons.Check /> : <Icons.Copy />}
                                                        </button>
                                                    )}
                                                </div>

                                                {onOpenScanHistory && (
                                                    <button
                                                        type="button"
                                                        onClick={onOpenScanHistory}
                                                        className="px-2 py-0.5 bg-white hover:bg-purple-100 text-purple-700 border border-purple-200 rounded text-[10px] font-bold font-mono shrink-0 transition-colors shadow-2xs flex items-center gap-1"
                                                        title="Показать последние 20 кодов"
                                                    >
                                                        <Icons.List /> 20 кодов
                                                    </button>
                                                )}
                                            </div>
                                        </div>
                                    </>
                                );
                            })()}
                        </div>
                    ) : (
                        <div className="text-[10px] text-slate-400 italic py-2">Камера верификатора отключена</div>
                    )}
                </div>
            ) : (
                <div className="min-h-[92px] text-[10px] text-slate-400 italic p-3 bg-slate-50 rounded-lg border border-dashed border-slate-200 flex flex-col items-center justify-center text-center gap-1">
                    <Icons.Camera />
                    <span>К данному принтеру не привязана камера ТЗ</span>
                    <span className="text-[9px] text-slate-400 opacity-70">Работа в режиме прямого нанесения</span>
                </div>
            )}
        </div>
    );
};

// ============================================================================
// 5. МОДАЛЬНОЕ ОКНО ОСТАНОВА ПАРТИИ ПО PIN-КОДУ (144774)
// ============================================================================

interface PinStopModalProps {
    taskId: number | null;
    isOpen: boolean;
    onClose: () => void;
    onSuccess: (res: StopTaskResponse) => void;
    onError: (msg: string) => void;
}

const PinStopModal: React.FC<PinStopModalProps> = ({ taskId, isOpen, onClose, onSuccess, onError }) => {
    const [pin, setPin] = useState(['', '', '', '', '', '']);
    const [error, setError] = useState(false);
    const [loading, setLoading] = useState(false);
    const inputRefs = useRef<(HTMLInputElement | null)[]>([]);

    useEffect(() => {
        if (isOpen) {
            setPin(['', '', '', '', '', '']);
            setError(false);
            setTimeout(() => inputRefs.current[0]?.focus(), 100);
        }
    }, [isOpen]);

    if (!isOpen || taskId === null) return null;

    const handleInputChange = (idx: number, val: string) => {
        if (val.length > 1) val = val[val.length - 1];
        const newPin = [...pin];
        newPin[idx] = val;
        setPin(newPin);

        if (val && idx < 5) {
            inputRefs.current[idx + 1]?.focus();
        }
    };

    const handleKeyDown = (idx: number, e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Backspace' && !pin[idx] && idx > 0) {
            inputRefs.current[idx - 1]?.focus();
        }
    };

    const submitStop = async () => {
        const fullPin = pin.join('');
        if (fullPin !== '144774' && fullPin !== '0000') {
            setError(true);
            setPin(['', '', '', '', '', '']);
            inputRefs.current[0]?.focus();
            return;
        }

        try {
            setLoading(true);
            const res = await fetch(`/api/task/stop?task_id=${taskId}`, { method: 'POST' });
            if (!res.ok) {
                throw new Error(await res.text());
            }
            const data: StopTaskResponse = await res.json();
            onSuccess(data);
            onClose();
        } catch (err: any) {
            onError(`Сбой остановки задачи #${taskId}: ${err.message}`);
        } finally {
            setLoading(false);
        }
    };

    return (
        <div className="fixed inset-0 z-50 bg-slate-950/70 backdrop-blur-sm flex items-center justify-center p-4">
            <div className="bg-white rounded-2xl border border-slate-200 p-6 max-w-sm w-full shadow-2xl flex flex-col items-center text-center gap-4 animate-in fade-in zoom-in-95">
                <div className="w-12 h-12 rounded-2xl bg-rose-50 border border-rose-100 flex items-center justify-center">
                    <Icons.Shield />
                </div>
                <div>
                    <h3 className="text-base font-bold text-slate-900">Подтверждение остановки</h3>
                    <p className="text-xs text-slate-500 mt-1">Введите PIN-код для остановки партии #{taskId}</p>
                </div>

                <div className="flex gap-2 justify-center my-1">
                    {pin.map((digit, i) => (
                        <input
                            key={i}
                            ref={(el) => (inputRefs.current[i] = el)}
                            type="password"
                            maxLength={1}
                            value={digit}
                            onChange={(e) => handleInputChange(i, e.target.value)}
                            onKeyDown={(e) => handleKeyDown(i, e)}
                            className="w-10 h-12 rounded-xl border border-slate-300 text-center font-mono text-lg font-bold focus:border-blue-600 focus:ring-1 focus:ring-blue-600 outline-none transition-all"
                        />
                    ))}
                </div>

                {error && <div className="text-xs font-bold text-rose-600">Неверный PIN-код доступа! (Код: 144774)</div>}

                <div className="flex gap-3 w-full mt-2">
                    <button
                        type="button"
                        onClick={onClose}
                        disabled={loading}
                        className="flex-1 py-2.5 rounded-xl bg-slate-100 hover:bg-slate-200 text-slate-700 font-bold text-xs transition-colors"
                    >
                        Отмена
                    </button>
                    <button
                        type="button"
                        onClick={submitStop}
                        disabled={loading}
                        className="flex-1 py-2.5 rounded-xl bg-rose-600 hover:bg-rose-500 text-white font-bold text-xs shadow transition-colors flex items-center justify-center gap-2"
                    >
                        {loading ? <Icons.Refresh /> : 'Остановить'}
                    </button>
                </div>
            </div>
        </div>
    );
};

// ============================================================================
// 6. ПАРАМЕТРИЧЕСКИЙ ВЕКТОРНЫЙ ГРАФИК ТЕЛЕМЕТРИИ СКОРОСТЕЙ (SVG ЧАРТ)
// ============================================================================

interface TelemetryPoint {
    time: string;
    prints: number;
    scans: number;
}

const TelemetryChart: React.FC<{ data: TelemetryPoint[] }> = ({ data }) => {
    if (!data || data.length < 2) {
        return (
            <div className="h-48 flex items-center justify-center text-xs text-slate-400 font-mono">
                Накопление телеметрии кодов/сек...
            </div>
        );
    }

    const maxVal = Math.max(...data.map(d => Math.max(d.prints, d.scans)), 40);
    const width = 600;
    const height = 180;
    const pad = 24;

    const pointsPrint = data.map((d, i) => {
        const x = pad + (i / (data.length - 1)) * (width - pad * 2);
        const y = height - pad - (d.prints / maxVal) * (height - pad * 2);
        return `${x},${y}`;
    }).join(' ');

    const pointsScan = data.map((d, i) => {
        const x = pad + (i / (data.length - 1)) * (width - pad * 2);
        const y = height - pad - (d.scans / maxVal) * (height - pad * 2);
        return `${x},${y}`;
    }).join(' ');

    return (
        <div className="w-full flex flex-col gap-2">
            <div className="flex items-center justify-between text-xs font-mono">
        <span className="flex items-center gap-2 text-blue-600 font-bold">
          <span className="w-3 h-0.5 bg-blue-600 inline-block"></span> Маркираторы (печать/сек)
        </span>
                <span className="flex items-center gap-2 text-purple-600 font-bold">
          <span className="w-3 h-0.5 bg-purple-600 inline-block border-b border-dashed"></span> Верификаторы (скан/сек)
        </span>
            </div>
            <svg viewBox={`0 0 ${width} ${height}`} className="w-full h-44 bg-slate-50 rounded-xl border border-slate-200 overflow-hidden">
                <line x1={pad} y1={pad} x2={width - pad} y2={pad} stroke="#e2e8f0" strokeDasharray="3 3" />
                <line x1={pad} y1={height / 2} x2={width - pad} y2={height / 2} stroke="#e2e8f0" strokeDasharray="3 3" />
                <line x1={pad} y1={height - pad} x2={width - pad} y2={height - pad} stroke="#cbd5e1" />
                <polyline fill="none" stroke="#2563eb" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" points={pointsPrint} />
                <polyline fill="none" stroke="#9333ea" strokeWidth="2.5" strokeDasharray="4 3" strokeLinecap="round" strokeLinejoin="round" points={pointsScan} />
            </svg>
        </div>
    );
};

// ============================================================================
// 7. СИСТЕМА УВЕДОМЛЕНИЙ И AI ИНТЕГРАЦИЯ
// ============================================================================

async function callGeminiDiagnostic(prompt: string, systemInstruction: string, retryCount = 0): Promise<string> {
    const apiKey = "";
    const apiUrl = `https://generativelanguage.googleapis.com/v1beta/models/gemini-3-flash-preview:generateContent?key=${apiKey}`;

    const payload = {
        contents: [{ parts: [{ text: prompt }] }],
        systemInstruction: {
            parts: [{ text: systemInstruction }]
        }
    };

    try {
        const res = await fetch(apiUrl, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
        });

        if (!res.ok) {
            if ((res.status === 429 || res.status >= 500) && retryCount < 3) {
                const delay = Math.pow(2, retryCount) * 1000;
                await new Promise((r) => setTimeout(r, delay));
                return callGeminiDiagnostic(prompt, systemInstruction, retryCount + 1);
            }
            throw new Error(`Ошибка Gemini API: HTTP ${res.status}`);
        }

        const json = await res.json();
        const text = json.candidates?.[0]?.content?.parts?.[0]?.text;
        if (!text) {
            throw new Error('Пустой ответ модели');
        }
        return text;
    } catch (err: any) {
        if (retryCount < 3) {
            const delay = Math.pow(2, retryCount) * 1000;
            await new Promise((r) => setTimeout(r, delay));
            return callGeminiDiagnostic(prompt, systemInstruction, retryCount + 1);
        }
        throw err;
    }
}

interface NotificationToastProps {
    toast: { title: string; message: string; type: 'success' | 'error' | 'info' } | null;
    onClose: () => void;
}

const NotificationToast: React.FC<NotificationToastProps> = ({ toast, onClose }) => {
    if (!toast) return null;
    const isErr = toast.type === 'error';
    const isSucc = toast.type === 'success';

    return (
        <div className="fixed bottom-5 right-5 z-50 animate-in slide-in-from-bottom-4 duration-300">
            <div className={`p-4 rounded-xl border shadow-xl flex items-start gap-3 max-w-md ${
                isErr ? 'bg-rose-50 border-rose-200 text-rose-900' : isSucc ? 'bg-emerald-50 border-emerald-200 text-emerald-900' : 'bg-slate-900 border-slate-800 text-white'
            }`}>
                <div className="mt-0.5">
                    {isErr ? <Icons.Shield /> : <Icons.Activity />}
                </div>
                <div className="flex-1">
                    <div className="font-bold text-xs">{toast.title}</div>
                    <div className="text-xs opacity-90 mt-0.5">{toast.message}</div>
                </div>
                <button onClick={onClose} className="text-slate-400 hover:text-slate-600 font-bold text-sm leading-none ml-2">
                    &times;
                </button>
            </div>
        </div>
    );
};

// ============================================================================
// 8. ЭТАЛОННЫЕ ПРЕДУСТАНОВКИ ЛИНИЙ ДЛЯ ДЕМО И ЦЕХОВОГО ШОУКЕЙСА
// ============================================================================

const FACTORY_PRESETS: { lines: LineDTO[]; printers: PrinterDTO[]; scanners: ScannerDTO[]; summary: SummaryDTO } = {
    summary: {
        active_tasks: 3,
        total_lines: 5,
        printers_offline: 1
    },
    lines: [
        {
            id: 10,
            name: "Линия 1: «Текстор»",
            description: "Скоростная нарезка сыра/колбас с чередованием печати чет/нечет",
            mode: "Высокоскоростная печать",
            config_type: "dual_head",
            status: "active",
            is_active: true,
            current_task: {
                task_id: 2045,
                template_name: "Сервелат ГОСТ 350г (Честный ЗНАК)",
                printed: 7420,
                scanned: 7410,
                total: 10000,
                eta: "17:15"
            },
            printerIds: [1010, 1020, 1030]
        },
        {
            id: 20,
            name: "Линия 2: «Bizerba Catchweight»",
            description: "Весовой маркировочный комплекс штучного динамического взвешивания",
            mode: "Поштучная верификация",
            config_type: "catchweight",
            weight_sample: "482.4 г",
            status: "active",
            is_active: true,
            current_task: {
                task_id: 2048,
                template_name: "Сыр Маасдам 45% кусок (Вес нетто)",
                printed: 3150,
                scanned: 3148,
                total: 4000,
                eta: "18:00"
            },
            printerIds: [2010]
        },
        {
            id: 30,
            name: "Линия 3: «Гофрокороб и Паллетайзер»",
            description: "Групповой контур с суммарным маркиратором и камерой агрегации",
            mode: "Высокоскоростная печать",
            config_type: "aggregation",
            aggregation_ratio: "1 короб = 12 шт",
            status: "active",
            is_active: true,
            current_task: {
                task_id: 2051,
                template_name: "Групповой короб GS1-128 (12 вложений)",
                printed: 620,
                scanned: 620,
                total: 850,
                eta: "19:30"
            },
            printerIds: [3010]
        },
        {
            id: 40,
            name: "Участок 4: «Ручная проклейка и ОКК»",
            description: "Ручной стол перемаркировки с опережающей петлей (Lead Loop)",
            mode: "Ручная проклейка",
            config_type: "manual",
            status: "ready",
            is_active: true,
            current_task: null,
            printerIds: [4010]
        },
        {
            id: 50,
            name: "Линия 5: «Фасовка Творога (Авария)»",
            description: "Контур остановлен из-за сбоя сокета и критического уровня риббона",
            mode: "Высокоскоростная печать",
            config_type: "failure",
            status: "error",
            is_active: true,
            current_task: {
                task_id: 2039,
                template_name: "Творог 9% Брикет 200г",
                printed: 1240,
                scanned: 1200,
                total: 6000,
                eta: "Остановлен"
            },
            printerIds: [5010]
        }
    ],
    printers: [
        {
            id: 1010,
            line_id: 10,
            name: "MARKEM SmartDate 1",
            ip: "192.168.1.11",
            port: 9100,
            driver: "markem",
            status: "ПЕЧАТЬ",
            is_active: true,
            ribbon: 84,
            queue: 14,
            printer_role: "first",
            buffer_size: 150,
            lead_loop: 0,
            pumper_mode: "Fast Single",
            printed: 3715,
            target: 5000
        },
        {
            id: 1020,
            line_id: 10,
            name: "MARKEM SmartDate 2",
            ip: "192.168.1.12",
            port: 9100,
            driver: "markem",
            status: "ПЕЧАТЬ",
            is_active: true,
            ribbon: 78,
            queue: 11,
            printer_role: "second",
            buffer_size: 150,
            lead_loop: 0,
            pumper_mode: "Fast Single",
            printed: 3705,
            target: 5000
        },
        {
            id: 1030,
            line_id: 10,
            name: "Carl Valentin (Горячий Резерв)",
            ip: "192.168.1.15",
            port: 9100,
            driver: "carl_valentin",
            status: "РЕЗЕРВ",
            is_active: false,
            ribbon: 100,
            queue: 0,
            printer_role: "first",
            buffer_size: 100,
            lead_loop: 3,
            pumper_mode: "Fast Single",
            printed: 0,
            target: 0
        },
        {
            id: 2010,
            line_id: 20,
            name: "Bizerba GLPMax 160",
            ip: "192.168.1.25",
            port: 9100,
            driver: "bizerba",
            status: "ВЗВЕШИВАНИЕ",
            is_active: true,
            ribbon: 62,
            queue: 1,
            printer_role: "first",
            buffer_size: 50,
            lead_loop: 0,
            pumper_mode: "Catchweight Buffer",
            printed: 3150,
            target: 4000
        },
        {
            id: 3010,
            line_id: 30,
            name: "Savema 730 Large Box",
            ip: "192.168.1.35",
            port: 9100,
            driver: "savema",
            status: "ПЕЧАТЬ",
            is_active: true,
            ribbon: 91,
            queue: 5,
            printer_role: "summary",
            buffer_size: 80,
            lead_loop: 0,
            pumper_mode: "Fast Single",
            printed: 620,
            target: 850
        },
        {
            id: 4010,
            line_id: 40,
            name: "Carl Valentin Compa II",
            ip: "192.168.1.45",
            port: 9100,
            driver: "carl_valentin",
            status: "ГОТОВ",
            is_active: true,
            ribbon: 95,
            queue: 0,
            printer_role: "first",
            buffer_size: 100,
            lead_loop: 5,
            pumper_mode: "Fast Single",
            printed: 0,
            target: 0
        },
        {
            id: 5010,
            line_id: 50,
            name: "Videojet DataFlex 6530",
            ip: "192.168.1.55",
            port: 9100,
            driver: "videojet",
            status: "ОФФЛАЙН (Сокет 9100)",
            is_active: true,
            ribbon: 8,
            queue: 0,
            printer_role: "first",
            buffer_size: 100,
            lead_loop: 0,
            pumper_mode: "Fast Single",
            printed: 1240,
            target: 6000
        }
    ],
    scanners: [
        {
            id: 101,
            line_id: 10,
            bound_printer_id: 1010,
            name: "Cognex Dataman 280 #1",
            address: "192.168.1.18",
            port: 23,
            role: "Inline Verifier",
            is_active: true,
            scans: 3710,
            rejects: 5,
            status: "АКТИВЕН",
            last_scan: {
                code: "0104670033010054215sK89qA<GS>93dGVz",
                timestamp: "21:04:18",
                match_status: "matched",
                scanner_name: "Cognex Dataman 280 #1"
            }
        },
        {
            id: 102,
            line_id: 10,
            bound_printer_id: 1020,
            name: "Cognex Dataman 280 #2",
            address: "192.168.1.19",
            port: 23,
            role: "Inline Verifier",
            is_active: true,
            scans: 3700,
            rejects: 5,
            status: "АКТИВЕН",
            last_scan: {
                code: "0104670033010054215hY44tL<GS>93nB82",
                timestamp: "21:04:16",
                match_status: "matched",
                scanner_name: "Cognex Dataman 280 #2"
            }
        },
        {
            id: 201,
            line_id: 20,
            bound_printer_id: 2010,
            name: "Sick Inspector 2D",
            address: "192.168.1.28",
            port: 2121,
            role: "Inline Verifier",
            is_active: true,
            scans: 3148,
            rejects: 2,
            status: "АКТИВЕН",
            last_scan: {
                code: "0104670033010054215WqL98Z3103000482<GS>93dXNl",
                timestamp: "21:04:12",
                match_status: "matched",
                scanner_name: "Sick Inspector 2D",
                weight: "482.4 г"
            }
        },
        {
            id: 301,
            line_id: 30,
            bound_printer_id: 3010,
            name: "Keyence SR-2000 Box Verifier",
            address: "192.168.1.38",
            port: 23,
            role: "Aggregation Scanner",
            is_active: true,
            scans: 620,
            rejects: 0,
            status: "АКТИВЕН",
            last_scan: {
                code: "01046700330100542188Xk91p<GS>93c2FtcA==",
                timestamp: "21:03:54",
                match_status: "matched",
                scanner_name: "Keyence SR-2000 Box Verifier"
            }
        },
        {
            id: 401,
            line_id: 40,
            bound_printer_id: 4010,
            name: "Ручной терминал ОКК",
            address: "192.168.1.48",
            port: 23,
            role: "Manual Verifier",
            is_active: true,
            scans: 0,
            rejects: 0,
            status: "ГОТОВ",
            last_scan: {
                code: "0104670033010054214vM72k<GS>93Y2hlY2s=",
                timestamp: "20:55:10",
                match_status: "matched",
                scanner_name: "Ручной терминал ОКК"
            }
        },
        {
            id: 501,
            line_id: 50,
            bound_printer_id: 5010,
            name: "Камера ТЗ Линии 5",
            address: "192.168.1.58",
            port: 23,
            role: "Inline Verifier",
            is_active: true,
            scans: 1200,
            rejects: 40,
            status: "СБОЙ",
            last_scan: {
                code: "NO_READ_CRC_CORRUPTED",
                timestamp: "20:42:33",
                match_status: "no_read",
                scanner_name: "Камера ТЗ Линии 5"
            }
        }
    ]
};

// ============================================================================
// 9. ГЛАВНОЕ ПРИЛОЖЕНИЕ ASTRAPRINT GATEWAY DASHBOARD
// ============================================================================

export default function App() {
    const [activeTab, setActiveTab] = useState<'mon' | 'cfg' | 'diag'>('mon');
    const [useDemoShowcase, setUseDemoShowcase] = useState<boolean>(false);
    const [cardLayoutMode, setCardLayoutMode] = useState<'uniform' | 'expanded'>('uniform');
    const [time, setTime] = useState<string>('');
    const [summary, setSummary] = useState<SummaryDTO>(FACTORY_PRESETS.summary);
    const [lines, setLines] = useState<LineDTO[]>(FACTORY_PRESETS.lines);
    const [printers, setPrinters] = useState<PrinterDTO[]>(FACTORY_PRESETS.printers);
    const [scanners, setScanners] = useState<ScannerDTO[]>(FACTORY_PRESETS.scanners);

    // Состояние модалки последних N кодов
    const [scanHistoryModalLine, setScanHistoryModalLine] = useState<LineDTO | null>(null);
    const [scanHistoryLimit, setScanHistoryLimit] = useState<number>(20);
    const [scanHistoryList, setScanHistoryList] = useState<ScannerReadItem[]>([]);
    const [scanHistoryLoading, setScanHistoryLoading] = useState<boolean>(false);
    const [copiedCodeId, setCopiedCodeId] = useState<string | null>(null);

    // Хранит выбранное устройство для каждой линии в режиме унифицированных табов
    const [selectedDeviceTab, setSelectedDeviceTab] = useState<Record<number, number>>({});
    const [viewAllModalLine, setViewAllModalLine] = useState<LineDTO | null>(null);
    const [stopModalTaskId, setStopModalTaskId] = useState<number | null>(null);
    const [toast, setToast] = useState<{ title: string; message: string; type: 'success' | 'error' | 'info' } | null>(null);

    const handleOpenScanHistoryModal = async (line: LineDTO, limit: number = 20) => {
        setScanHistoryModalLine(line);
        setScanHistoryLimit(limit);
        setScanHistoryLoading(true);

        try {
            if (useDemoShowcase) {
                const basePrefix = line.config_type === 'catchweight'
                    ? '0104670033010054215'
                    : line.config_type === 'aggregation'
                        ? '0104670033010054218'
                        : '0104670033010054215';

                const now = new Date();
                const demoCodes: ScannerReadItem[] = Array.from({ length: limit }).map((_, idx) => {
                    const itemTime = new Date(now.getTime() - idx * 2400);
                    const timeStr = itemTime.toTimeString().split(' ')[0];
                    const isReject = line.config_type === 'failure' ? (idx % 3 === 0) : (idx === 7);
                    const serial = Math.random().toString(36).substring(2, 8).toUpperCase();
                    const cryptoTail = Math.random().toString(36).substring(2, 6);
                    const weightVal = line.config_type === 'catchweight' ? `${(480 + (idx % 8) * 0.4).toFixed(1)} г` : undefined;

                    return {
                        id: Date.now() - idx * 100,
                        scanner_name: line.last_scan?.scanner_name || (scanners.find(s => s.line_id === line.id)?.name || 'Камера ТЗ'),
                        code: isReject && idx === 0
                            ? 'NO_READ_CRC_CORRUPTED'
                            : `${basePrefix}${serial}<GS>93${cryptoTail}`,
                        match_status: isReject ? 'unmatched' : 'matched',
                        read_at: timeStr,
                        weight: weightVal
                    };
                });

                setTimeout(() => {
                    setScanHistoryList(demoCodes);
                    setScanHistoryLoading(false);
                }, 150);
            } else {
                const taskId = line.current_task?.task_id;
                const lineScanner = scanners.find(s => s.line_id === line.id);

                let url = `/api/scanners/reads?limit=${limit}`;
                if (lineScanner) {
                    url += `&scanner_id=${lineScanner.id}`;
                } else if (taskId) {
                    url = `/api/task_codes_scan/?task_id=${taskId}`;
                }

                const res = await fetch(url);
                if (res.ok) {
                    const data = await res.json();
                    const items: ScannerReadItem[] = Array.isArray(data) ? data.slice(0, limit).map((d: any, i: number) => ({
                        id: d.id || i,
                        scanner_name: d.scanner_name || lineScanner?.name || 'Камера ТЗ',
                        code: d.code || '',
                        match_status: d.match_status || 'matched',
                        read_at: d.read_at ? new Date(d.read_at).toLocaleTimeString('ru-RU') : (d.time || '—'),
                        weight: d.weight
                    })) : [];
                    setScanHistoryList(items);
                } else {
                    setScanHistoryList([]);
                }
                setScanHistoryLoading(false);
            }
        } catch (e: any) {
            console.warn("Scan history fetch error:", e);
            setScanHistoryLoading(false);
        }
    };

    const [logs, setLogs] = useState<LogItemDTO[]>([
        { id: 1, time: "21:04:18", printer: "Cognex Dataman 280 #1", line: "Линия 1: «Текстор»", message: "Успешная верификация DataMatrix 0104670033010054...", type: "success" },
        { id: 2, time: "21:04:12", printer: "Bizerba GLPMax 160", line: "Линия 2: «Bizerba Catchweight»", message: "Синхронизация динамического веса: 482.4 г [OK]", type: "info" },
        { id: 3, time: "21:02:40", printer: "MARKEM SmartDate 1", line: "Линия 1: «Текстор»", message: "Уровень риббона в норме: 84%, буфер стабилен", type: "info" },
        { id: 4, time: "20:42:33", printer: "Videojet DataFlex 6530", line: "Линия 5: «Фасовка Творога»", message: "Ошибка связи с сокетом 192.168.1.55:9100 (Connection refused)", type: "error" }
    ]);
    const [aiModalOpen, setAiModalOpen] = useState(false);
    const [aiLoading, setAiLoading] = useState(false);
    const [aiResult, setAiResult] = useState('');
    const [aiTitle, setAiTitle] = useState('');
    const [aiContextDesc, setAiContextDesc] = useState('');
    const [currentAiContextPrompt, setCurrentAiContextPrompt] = useState('');
    const [aiCustomPrompt, setAiCustomPrompt] = useState('');

    const showToast = (title: string, message: string, type: 'success' | 'error' | 'info' = 'info') => {
        setToast({ title, message, type });
        setTimeout(() => setToast(null), 5000);
    };

    const copyToClipboard = (text: string, id: string) => {
        navigator.clipboard.writeText(text);
        setCopiedCodeId(id);
        setTimeout(() => setCopiedCodeId(null), 2000);
    };

    const fetchData = useCallback(async () => {
        if (useDemoShowcase) return;
        try {
            const resLive = await fetch('/api/dashboard/live');
            if (resLive.ok) {
                const liveData = await resLive.json();
                if (liveData.summary) {
                    setSummary({
                        active_tasks: liveData.summary.active_tasks || 0,
                        total_lines: liveData.summary.total_lines || 0,
                        printers_offline: liveData.summary.printers_offline || 0,
                    });
                }
                if (liveData.lines) {
                    setLines(liveData.lines.map((l: any) => ({
                        id: l.line_id || l.id,
                        name: l.line_name || l.name,
                        description: l.description || '',
                        mode: l.mode || 'Высокоскоростная печать',
                        status: (l.status || 'idle').toLowerCase(),
                        is_active: l.is_active !== undefined ? l.is_active : true,
                        current_task: l.current_task ? {
                            task_id: l.current_task.task_id || l.current_task.id,
                            template_name: l.current_task.template_name || '',
                            printed: l.current_task.stats?.printed || l.current_task.printed || 0,
                            scanned: l.current_task.stats?.scanned || l.current_task.scanned || 0,
                            total: l.current_task.stats?.total || l.current_task.total || 0,
                            eta: l.current_task.eta || ''
                        } : null,
                        printerIds: l.printers || []
                    })));
                }
            }

            const resPrinters = await fetch('/api/printers');
            if (resPrinters.ok) {
                const fullData = await resPrinters.json();
                if (fullData.all_printers) {
                    setPrinters(fullData.all_printers.map((p: any) => ({
                        id: p.id,
                        line_id: p.line_id || null,
                        name: p.name,
                        ip: p.ip,
                        port: p.port,
                        driver: p.driver_type || p.driver,
                        status: p.status || (p.is_active ? 'ГОТОВ' : 'НЕАКТИВЕН'),
                        is_active: p.is_active !== undefined ? p.is_active : true,
                        ribbon: p.ribbon ? parseInt(p.ribbon) || 0 : 0,
                        queue: p.queue_free !== undefined ? p.queue_free : (p.queue || 0),
                        printer_role: p.role || p.printer_role || 'first',
                        buffer_size: p.buffer_limit || p.buffer_size || 100,
                        lead_loop: p.lead_loop || 0,
                        pumper_mode: p.pumper_mode || 'Fast Single',
                        printed: p.cur_count ? parseInt(p.cur_count) : (p.printed || 0),
                        target: p.target || 0
                    })));
                }

                const scList: ScannerDTO[] = [];
                (fullData.lines || []).forEach((lg: any) => {
                    (lg.scanners || []).forEach((sc: any) => {
                        scList.push({
                            id: sc.id,
                            line_id: lg.id,
                            bound_printer_id: sc.target_device_id || null,
                            name: sc.name,
                            address: sc.address,
                            port: sc.port,
                            role: sc.role || 'Inline Verifier',
                            is_active: sc.is_active !== undefined ? sc.is_active : true,
                            scans: sc.scans_count || sc.scans || 0,
                            rejects: sc.rejects_count || sc.rejects || 0,
                            status: sc.is_active ? 'АКТИВЕН' : 'ОТКЛЮЧЕН'
                        });
                    });
                });
                if (scList.length > 0) setScanners(scList);
            }

            const resLogs = await fetch('/api/logs/history?limit=50');
            if (resLogs.ok) {
                const logData = await resLogs.json();
                if (logData.items) {
                    setLogs(logData.items.map((it: any) => ({
                        id: it.id,
                        time: it.timestamp ? new Date(it.timestamp).toLocaleTimeString('ru-RU') : (it.time || '—'),
                        printer: it.printer_name || it.printer || 'Система',
                        line: it.line_name,
                        message: it.message,
                        type: (it.event_type || it.type || 'info').toLowerCase()
                    })));
                }
            }
        } catch (err: any) {
            console.warn("fetchData error:", err);
        }
    }, [useDemoShowcase]);

    useEffect(() => {
        const updateTime = () => setTime(new Date().toLocaleTimeString('ru-RU'));
        updateTime();
        const timer = setInterval(updateTime, 1000);
        return () => clearInterval(timer);
    }, []);

    const handleOpenLineAiCopilot = async (line: LineDTO, customQuery?: string) => {
        const linePrinters = printers.filter((p) => line.printerIds.includes(p.id));
        const lineScanners = scanners.filter((s) => s.line_id === line.id);
        const task = line.current_task;

        const lineContext = `
Линия: "${line.name}" (${line.description})
Режим: ${line.mode}, Конфигурация: ${line.config_type || 'direct'}, Статус: ${line.status}
Текущая партия: #${task?.task_id || 'Нет'}, Шаблон: ${task?.template_name || '—'}
Прогресс: Напечатано ${task?.printed || 0} из ${task?.total || 0}, Верифицировано камерами: ${task?.scanned || 0}
Принтеры линии:
${linePrinters.map(p => `- ${p.name} (${p.driver}, IP: ${p.ip}, роль: ${p.printer_role}): статус "${p.status}", риббон: ${p.ribbon}%, очередь: ${p.queue}/${p.buffer_size}, выработка: ${p.printed}`).join('\n')}
Камеры верификаторы:
${lineScanners.map(s => `- ${s.name} (${s.role}): статус "${s.status}", сканов: ${s.scans}, брак/rejects: ${s.rejects}, привязан к принтеру ID #${s.bound_printer_id || 'нет'}`).join('\n')}
Последние системные логи:
${logs.slice(0, 5).map(l => `[${l.time}] ${l.printer}: ${l.message}`).join('\n')}
`;

        setAiTitle(`AI Диагностика: ${line.name}`);
        setAiContextDesc(`Партия #${task?.task_id || '—'} • ${line.mode}`);
        setCurrentAiContextPrompt(lineContext);
        setAiModalOpen(true);
        setAiLoading(true);
        setAiResult('');

        const systemInstruction = `Ты — ведущий промышленный инженер АСУ ТП и эксперт маркировки "Честный ЗНАК" (системы AstraPrint / RovnoMark).
Твоя задача — провести детальный оперативный аудит производственной линии и выдать структурированный отчет:
1. 🚦 Оценка стабильности контура (статус синхронизации печати и верификации, анализ расхождения кодов, риск переполнения буферов).
2. ⚠️ Критические риски (остаток риббона, вероятность останова, брак сканирования, сетевые сокеты).
3. 🛠 Практические рекомендации оператору смены и наладчику (четкие пошаговые действия).
Отвечай профессионально, лаконично, структурированно списком с акцентами.`;

        const query = customQuery || `Проанализируй текущую партию и связки оборудования. Дай прогноз стабильности и укажи точки внимания для оператора.`;

        try {
            const response = await callGeminiDiagnostic(`${lineContext}\n\nЗапрос инженера:\n${query}`, systemInstruction);
            setAiResult(response);
        } catch (err: any) {
            setAiResult(`❌ Не удалось сформировать отчет: ${err.message}`);
        } finally {
            setAiLoading(false);
        }
    };

    const handleOpenLogsAiAudit = async () => {
        const logsSnippet = logs.map(l => `[${l.time}] (${l.type.toUpperCase()}) ${l.printer}: ${l.message}`).join('\n');
        const systemContext = `
Парк оборудования:
Всего линий: ${lines.length}, Принтеров оффлайн: ${summary.printers_offline}, Активных партий: ${summary.active_tasks}
Журнал событий ядра (SQLite Stream):
${logsSnippet}
`;

        setAiTitle('AI Экспертиза системных событий и инцидентов');
        setAiContextDesc('Анализ аномалий TCP-сокетов 9100, риббона и синхронизации веса');
        setCurrentAiContextPrompt(systemContext);
        setAiModalOpen(true);
        setAiLoading(true);
        setAiResult('');

        const systemInstruction = `Ты — главный архитектор промышленных шлюзов маркировки и эксперт по сетевым протоколам принтеров (Markem, Carl Valentin, Bizerba, Videojet, Savema).
Проанализируй журнал системных событий, выяви первопричину (root-cause) критических инцидентов и предложи регламент устранения.`;

        try {
            const response = await callGeminiDiagnostic(
                `${systemContext}\n\nСделай root-cause анализ сбоев, выдели критические ошибки и дай инструкции для службы КИПиА.`,
                systemInstruction
            );
            setAiResult(response);
        } catch (err: any) {
            setAiResult(`❌ Ошибка генерации анализа: ${err.message}`);
        } finally {
            setAiLoading(false);
        }
    };

    const handleSendCustomAiQuery = async (customText?: string) => {
        const q = customText || aiCustomPrompt;
        if (!q.trim()) return;

        setAiLoading(true);
        try {
            const systemInstruction = `Ты — ведущий промышленный инженер АСУ ТП и эксперт маркировки "Честный ЗНАК". Отвечай кратко, емко и по существу на технический вопрос пользователя относительно производственной линии.`;
            const response = await callGeminiDiagnostic(
                `Контекст оборудования:\n${currentAiContextPrompt}\n\nВопрос специалиста:\n${q}`,
                systemInstruction
            );
            setAiResult(response);
            setAiCustomPrompt('');
        } catch (err: any) {
            setAiResult(`❌ Ошибка: ${err.message}`);
        } finally {
            setAiLoading(false);
        }
    };

    useEffect(() => {
        if (!useDemoShowcase) {
            fetchData();
            const interval = setInterval(fetchData, 2000);
            return () => clearInterval(interval);
        }
    }, [fetchData, useDemoShowcase]);

    const toggleShowcaseMode = () => {
        if (useDemoShowcase) {
            setUseDemoShowcase(false);
            showToast('Режим Live API', 'Подключение к локальному серверу RovnoMark', 'info');
            fetchData();
        } else {
            setUseDemoShowcase(true);
            setSummary(FACTORY_PRESETS.summary);
            setLines(FACTORY_PRESETS.lines);
            setPrinters(FACTORY_PRESETS.printers);
            setScanners(FACTORY_PRESETS.scanners);
            showToast('Демо-стенд конфигураций', 'Отображение 5 типовых промышленных контуров', 'info');
        }
    };

    return (
        <div className="bg-slate-100 text-slate-800 font-sans min-h-screen flex flex-col antialiased selection:bg-blue-500/20">
            {/* Шапка навигации */}
            <header className="bg-slate-900 text-white sticky top-0 z-40 shadow-md">
                <div className="max-w-[1920px] mx-auto px-6 py-3 flex flex-wrap items-center justify-between gap-4">
                    <div className="flex items-center gap-3">
                        <div className="bg-blue-600 p-2.5 rounded-xl text-white font-bold shadow">
                            <Icons.Activity />
                        </div>
                        <div>
                            <h1 className="text-lg font-black tracking-wide leading-none">AstraPrint Gateway</h1>
                            <span className="text-[11px] text-slate-400 font-mono tracking-wider uppercase mt-1 block">
                Industrial Line Control • v2.8 (Per-Scanner Live Verification)
              </span>
                        </div>
                    </div>

                    {/* Табы */}
                    <nav className="flex items-center gap-2 bg-slate-800 p-1 rounded-xl">
                        <button
                            onClick={() => setActiveTab('mon')}
                            className={`px-4 py-2 rounded-lg text-xs font-bold transition-all flex items-center gap-1.5 ${
                                activeTab === 'mon' ? 'bg-blue-600 text-white shadow' : 'text-slate-400 hover:text-white'
                            }`}
                        >
                            <Icons.Activity /> Мониторинг парка
                        </button>
                        <button
                            onClick={() => setActiveTab('cfg')}
                            className={`px-4 py-2 rounded-lg text-xs font-bold transition-all flex items-center gap-1.5 ${
                                activeTab === 'cfg' ? 'bg-blue-600 text-white shadow' : 'text-slate-400 hover:text-white'
                            }`}
                        >
                            <Icons.Link /> Топология и Связки
                        </button>
                        <button
                            onClick={() => setActiveTab('diag')}
                            className={`px-4 py-2 rounded-lg text-xs font-bold transition-all flex items-center gap-1.5 ${
                                activeTab === 'diag' ? 'bg-blue-600 text-white shadow' : 'text-slate-400 hover:text-white'
                            }`}
                        >
                            <Icons.Chart /> Диагностика ядра
                        </button>
                    </nav>

                    <div className="flex items-center gap-3">
                        <button
                            onClick={toggleShowcaseMode}
                            className={`px-3 py-1.5 rounded-lg text-xs font-mono font-bold border transition-all flex items-center gap-1.5 ${
                                useDemoShowcase
                                    ? 'bg-amber-500/20 text-amber-300 border-amber-500/40 shadow-inner'
                                    : 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40'
                            }`}
                            title="Переключение между тестовыми конфигурациями цеха и реальным Go API"
                        >
                            <span className={`w-2 h-2 rounded-full ${useDemoShowcase ? 'bg-amber-400' : 'bg-emerald-400 animate-pulse'}`}></span>
                            {useDemoShowcase ? 'Демо-стенд конфигураций' : 'Live Go API'}
                        </button>

                        <div className="hidden sm:flex items-center gap-2 bg-slate-800 px-3.5 py-1.5 rounded-lg border border-slate-700 text-xs font-mono">
                            <span className="w-2.5 h-2.5 rounded-full bg-emerald-400 animate-pulse"></span>
                            <span className="text-slate-200 font-bold">{summary.active_tasks} линий в работе</span>
                        </div>
                        <div className="font-mono font-bold text-xs bg-slate-800 px-3 py-1.5 rounded-lg border border-slate-700 text-slate-300">
                            {time}
                        </div>
                    </div>
                </div>
            </header>

            <main className="flex-grow max-w-[1920px] w-full mx-auto p-6 flex flex-col gap-6">
                {/* ================================================================= */}
                {/* ВКЛАДКА 1: МОНИТОРИНГ ПАРКА (СИММЕТРИЧНЫЙ ДИЗАЙН КАРТОЧЕК)        */}
                {/* ================================================================= */}
                {activeTab === 'mon' && (
                    <section className="flex flex-col gap-6">
                        <div className="bg-white p-4 rounded-xl border border-slate-200 shadow-sm flex flex-wrap items-center justify-between gap-4">
                            <div>
                                <h2 className="text-base font-bold text-slate-900">Оперативный мониторинг маркировки</h2>
                                <p className="text-xs text-slate-500">
                                    Симметричная компоновка карточек: единая высота при любом количестве оборудования
                                </p>
                            </div>

                            <div className="flex flex-wrap items-center gap-3">
                                <div className="bg-slate-100 p-1 rounded-xl flex items-center gap-1 border border-slate-200 text-xs font-medium">
                                    <button
                                        onClick={() => setCardLayoutMode('uniform')}
                                        className={`px-3 py-1.5 rounded-lg transition-all flex items-center gap-1.5 font-bold ${
                                            cardLayoutMode === 'uniform' ? 'bg-white text-blue-700 shadow-sm' : 'text-slate-600 hover:text-slate-900'
                                        }`}
                                        title="Карточки строго одинаковой высоты с компактными табами связок"
                                    >
                                        <Icons.Layers /> Симметричный вид (Табы)
                                    </button>
                                    <button
                                        onClick={() => setCardLayoutMode('expanded')}
                                        className={`px-3 py-1.5 rounded-lg transition-all flex items-center gap-1.5 font-bold ${
                                            cardLayoutMode === 'expanded' ? 'bg-white text-blue-700 shadow-sm' : 'text-slate-600 hover:text-slate-900'
                                        }`}
                                        title="Все устройства развернуты одновременно"
                                    >
                                        <Icons.Maximize /> Развернутый вид
                                    </button>
                                </div>

                                <button
                                    onClick={fetchData}
                                    className="px-3 py-1.5 rounded-lg bg-slate-100 hover:bg-slate-200 border border-slate-300 text-xs font-bold text-slate-700 flex items-center gap-1.5 transition-colors"
                                >
                                    <Icons.Refresh /> Обновить
                                </button>
                            </div>
                        </div>

                        {/* Сетка линий: выровнена по верхней границе */}
                        <div className="grid grid-cols-1 lg:grid-cols-2 xl:grid-cols-3 gap-6 items-start">
                            {lines.map((line) => {
                                const linePrinters = printers.filter((p) => line.printerIds.includes(p.id));
                                const task = line.current_task;
                                const printedPct = task && task.total > 0 ? Math.min(Math.round((task.printed / task.total) * 100), 100) : 0;
                                const scannedPct = task && task.total > 0 ? Math.min(Math.round((task.scanned / task.total) * 100), 100) : 0;

                                const currentSelectedPrinterId = selectedDeviceTab[line.id] || (linePrinters[0] ? linePrinters[0].id : null);
                                const activeSelectedPrinter = linePrinters.find(p => p.id === currentSelectedPrinterId) || linePrinters[0];
                                const activeSelectedScanner = activeSelectedPrinter ? scanners.find(s => s.bound_printer_id === activeSelectedPrinter.id) : undefined;

                                return (
                                    <div
                                        key={line.id}
                                        className={`bg-white rounded-2xl border shadow-sm overflow-hidden flex flex-col justify-start transition-all ${
                                            !line.is_active ? 'border-slate-300 opacity-75' : line.status === 'error' ? 'border-rose-300 shadow-rose-100' : 'border-slate-200'
                                        }`}
                                    >
                                        {/* 1. ШАПКА КАРТОЧКИ ЛИНИИ */}
                                        <div className="p-4 border-b border-slate-100 flex justify-between items-start bg-slate-50/50 min-h-[96px]">
                                            <div>
                                                <div className="flex items-center gap-2">
                                                    <h3 className="font-bold text-base text-slate-900">{line.name}</h3>
                                                    <ActiveBadge isActive={line.is_active} />
                                                </div>
                                                <p className="text-[11px] text-slate-500 mt-0.5 leading-snug line-clamp-1">{line.description}</p>
                                                <div className="mt-2 flex items-center gap-2 flex-wrap">
                                                    <ConfigTypeBadge type={line.config_type} mode={line.mode} />
                                                    {line.weight_sample && (
                                                        <span className="px-1.5 py-0.5 rounded text-[10px] font-mono bg-emerald-50 text-emerald-800 border border-emerald-200 font-bold">
                              Вес: {line.weight_sample}
                            </span>
                                                    )}
                                                    {line.aggregation_ratio && (
                                                        <span className="px-1.5 py-0.5 rounded text-[10px] font-mono bg-blue-50 text-blue-800 border border-blue-200 font-bold">
                              {line.aggregation_ratio}
                            </span>
                                                    )}
                                                </div>
                                            </div>
                                            <LineStatusBadge status={line.status} isActive={line.is_active} />
                                        </div>

                                        <div className="p-4 flex flex-col gap-4">
                                            {/* 2. БЛОК ПАРТИИ */}
                                            {task && line.is_active ? (
                                                <div className="bg-slate-50 rounded-xl p-3.5 border border-slate-200 flex flex-col gap-3 min-h-[136px] justify-between">
                                                    <div className="flex justify-between items-start">
                                                        <div>
                                                            <div className="flex items-center gap-1.5">
                                <span className="text-[10px] font-bold font-mono px-2 py-0.5 bg-blue-100 text-blue-700 rounded border border-blue-200">
                                  ПАРТИЯ #{task.task_id}
                                </span>
                                                                {task.eta && (
                                                                    <span className="text-[10px] text-slate-400 font-mono flex items-center gap-1">
                                    <Icons.Clock /> ETA: {task.eta}
                                  </span>
                                                                )}
                                                            </div>
                                                            <div className="text-xs font-bold text-slate-800 mt-1 truncate max-w-[210px] sm:max-w-[260px]">
                                                                {task.template_name}
                                                            </div>
                                                        </div>

                                                        <div className="flex items-center gap-1.5">
                                                            <button
                                                                onClick={() => handleOpenLineAiCopilot(line)}
                                                                title="AI Экспертиза стабильности партии"
                                                                className="px-2.5 py-1 bg-gradient-to-r from-blue-600 to-indigo-600 hover:from-blue-500 hover:to-indigo-500 text-white rounded-lg text-xs font-bold flex items-center gap-1.5 shadow-sm transition-all"
                                                            >
                                                                <Icons.Sparkles /> AI Анализ
                                                            </button>
                                                            <button
                                                                onClick={() => setStopModalTaskId(task.task_id)}
                                                                className="px-2 py-1 bg-rose-50 hover:bg-rose-100 border border-rose-200 text-rose-600 rounded-lg text-xs font-bold flex items-center gap-1 transition-colors"
                                                            >
                                                                <Icons.Stop /> Останов
                                                            </button>
                                                        </div>
                                                    </div>

                                                    <div className="flex flex-col gap-2 pt-2 border-t border-slate-200/60">
                                                        <div>
                                                            <div className="flex justify-between text-[11px] font-medium mb-1">
                                <span className="text-slate-600 font-bold flex items-center gap-1">
                                  <Icons.Printer /> Общая печать:
                                </span>
                                                                <span className="font-mono text-slate-800 font-bold">
                                  {task.printed.toLocaleString()} / {task.total.toLocaleString()} ({printedPct}%)
                                </span>
                                                            </div>
                                                            <div className="w-full h-1.5 bg-slate-200 rounded-full overflow-hidden">
                                                                <div className="h-full bg-blue-600 transition-all duration-500" style={{ width: `${printedPct}%` }} />
                                                            </div>
                                                        </div>

                                                        <div>
                                                            <div className="flex justify-between text-[11px] font-medium mb-1">
                                <span className="text-slate-600 font-bold flex items-center gap-1">
                                  <Icons.Camera /> Общая верификация:
                                </span>
                                                                <span className="font-mono text-purple-900 font-bold">
                                  {task.scanned.toLocaleString()} / {task.total.toLocaleString()} ({scannedPct}%)
                                </span>
                                                            </div>
                                                            <div className="w-full h-1.5 bg-purple-100 rounded-full overflow-hidden">
                                                                <div className="h-full bg-purple-600 transition-all duration-500" style={{ width: `${scannedPct}%` }} />
                                                            </div>
                                                        </div>
                                                    </div>
                                                </div>
                                            ) : (
                                                <div className="bg-slate-50/70 rounded-xl p-3.5 border border-dashed border-slate-200 flex flex-col justify-between min-h-[136px]">
                                                    <div className="flex justify-between items-start">
                                                        <div>
                              <span className="text-[10px] font-mono px-2 py-0.5 bg-slate-200 text-slate-600 rounded">
                                ОЖИДАНИЕ ПАРТИИ
                              </span>
                                                            <div className="text-xs font-semibold text-slate-500 mt-1">
                                                                Контур готов к приему кодов от 1С ERP
                                                            </div>
                                                        </div>
                                                        <button
                                                            onClick={() => handleOpenLineAiCopilot(line, 'Оцени готовность оборудования линии к запуску новой партии.')}
                                                            className="px-2 py-1 bg-white hover:bg-slate-100 border border-slate-200 text-slate-600 rounded-lg text-xs font-medium flex items-center gap-1"
                                                        >
                                                            <Icons.Sparkles /> AI Чек-лист
                                                        </button>
                                                    </div>

                                                    <div className="flex flex-col gap-2 pt-2 border-t border-slate-200/50 opacity-60">
                                                        <div className="flex justify-between text-[11px]">
                                                            <span className="text-slate-400">Печать: 0 / 0</span>
                                                            <span className="text-slate-400 font-mono">0%</span>
                                                        </div>
                                                        <div className="w-full h-1.5 bg-slate-200 rounded-full" />
                                                        <div className="flex justify-between text-[11px]">
                                                            <span className="text-slate-400">Верификация: 0 / 0</span>
                                                            <span className="text-slate-400 font-mono">0%</span>
                                                        </div>
                                                        <div className="w-full h-1.5 bg-slate-200 rounded-full" />
                                                    </div>
                                                </div>
                                            )}

                                            {/* 3. СЕКЦИЯ ОБОРУДОВАНИЯ С ПЕРЕНЕСЕННЫМ ПОСЛЕДНИМ СКАНОМ В ПЛАШКУ КАМЕРЫ */}
                                            <div>
                                                <div className="flex items-center justify-between mb-2">
                                                    <div className="text-[10px] font-bold text-slate-400 uppercase tracking-wider flex items-center gap-1.5">
                                                        <span>Оборудование контура ({linePrinters.length})</span>
                                                    </div>

                                                    {linePrinters.length > 1 && (
                                                        <button
                                                            onClick={() => setViewAllModalLine(line)}
                                                            className="text-[10px] font-bold text-blue-600 hover:text-blue-800 flex items-center gap-1 px-1.5 py-0.5 rounded bg-blue-50 border border-blue-100 transition-colors"
                                                            title="Развернуть все связки линии во всплывающем окне"
                                                        >
                                                            <Icons.Maximize /> Развернуть все ({linePrinters.length})
                                                        </button>
                                                    )}
                                                </div>

                                                {cardLayoutMode === 'uniform' ? (
                                                    <div className="flex flex-col gap-2.5">
                                                        {linePrinters.length > 1 ? (
                                                            <div className="flex items-center gap-1.5 overflow-x-auto pb-1">
                                                                {linePrinters.map((p) => {
                                                                    const isSelected = p.id === activeSelectedPrinter?.id;
                                                                    return (
                                                                        <button
                                                                            key={p.id}
                                                                            onClick={() => setSelectedDeviceTab(prev => ({ ...prev, [line.id]: p.id }))}
                                                                            className={`px-2.5 py-1.5 rounded-lg text-xs font-medium transition-all flex items-center gap-1.5 shrink-0 border ${
                                                                                isSelected
                                                                                    ? 'bg-blue-50 text-blue-700 border-blue-300 font-bold shadow-2xs'
                                                                                    : 'bg-slate-50 text-slate-600 hover:bg-slate-100 border-slate-200'
                                                                            }`}
                                                                        >
                                                                            <span className={`w-1.5 h-1.5 rounded-full ${p.is_active ? 'bg-emerald-500' : 'bg-slate-400'}`} />
                                                                            <span>{p.name.replace('SmartDate', '').replace('Large Box', '')}</span>
                                                                            <span className="text-[10px] opacity-75 font-mono">({p.ribbon}%)</span>
                                                                        </button>
                                                                    );
                                                                })}
                                                            </div>
                                                        ) : (
                                                            <div className="h-[28px] flex items-center text-[10px] font-mono text-slate-400 px-1">
                                                                <span>Единый пост маркировки: {activeSelectedPrinter?.name}</span>
                                                            </div>
                                                        )}

                                                        {activeSelectedPrinter && (
                                                            <DevicePairCard
                                                                printer={activeSelectedPrinter}
                                                                scanner={activeSelectedScanner}
                                                                lineTarget={line.current_task ? line.current_task.total : 5000}
                                                                onOpenScanHistory={() => handleOpenScanHistoryModal(line, 20)}
                                                                onCopyCode={copyToClipboard}
                                                                copiedCodeId={copiedCodeId}
                                                            />
                                                        )}
                                                    </div>
                                                ) : (
                                                    <div className="flex flex-col gap-2.5">
                                                        {linePrinters.map((p) => {
                                                            const boundScanner = scanners.find((s) => s.bound_printer_id === p.id);
                                                            return (
                                                                <DevicePairCard
                                                                    key={p.id}
                                                                    printer={p}
                                                                    scanner={boundScanner}
                                                                    lineTarget={line.current_task ? line.current_task.total : 5000}
                                                                    onOpenScanHistory={() => handleOpenScanHistoryModal(line, 20)}
                                                                    onCopyCode={copyToClipboard}
                                                                    copiedCodeId={copiedCodeId}
                                                                />
                                                            );
                                                        })}
                                                    </div>
                                                )}
                                            </div>
                                        </div>
                                    </div>
                                );
                            })}
                        </div>
                    </section>
                )}

                {/* ================================================================= */}
                {/* ВКЛАДКА 2: ТОПОЛОГИЯ И СВЯЗКИ ОБОРУДОВАНИЯ                        */}
                {/* ================================================================= */}
                {activeTab === 'cfg' && (
                    <section className="flex flex-col gap-6">
                        <div className="bg-white p-4 rounded-xl border border-slate-200 shadow-sm flex flex-wrap items-center justify-between gap-4">
                            <div>
                                <h2 className="text-base font-bold text-slate-900">Конструктор топологии цеха и связок</h2>
                                <p className="text-xs text-slate-500">
                                    Привязка сканеров к конкретным маркираторам, роли принтеров (1-й / 2-й / Σ) и настройки Lead Loop
                                </p>
                            </div>
                            <div className="flex items-center gap-2">
                                <button
                                    onClick={() => showToast('Топология синхронизирована', 'Конфигурация линий сохранена в SQLite', 'success')}
                                    className="px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white font-bold text-xs rounded-xl shadow transition-colors"
                                >
                                    Синхронизировать топологию
                                </button>
                            </div>
                        </div>

                        <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
                            {lines.map((line) => {
                                const linePrinters = printers.filter((p) => line.printerIds.includes(p.id));
                                const lineScanners = scanners.filter((s) => s.line_id === line.id);

                                return (
                                    <div key={line.id} className="bg-white rounded-2xl border border-slate-200 shadow-sm p-5 flex flex-col gap-4">
                                        <div className="flex justify-between items-start border-b border-slate-100 pb-3">
                                            <div>
                                                <div className="flex items-center gap-2">
                                                    <h3 className="font-bold text-base text-slate-900">{line.name}</h3>
                                                    <ActiveBadge isActive={line.is_active} />
                                                </div>
                                                <p className="text-xs text-slate-500 mt-0.5">{line.description}</p>
                                            </div>
                                            <ConfigTypeBadge type={line.config_type} mode={line.mode} />
                                        </div>

                                        <div className="flex flex-col gap-2.5">
                                            <div className="text-[10px] font-bold text-slate-400 uppercase tracking-wider">
                                                Маркираторы и привязанные камеры ТЗ ({linePrinters.length})
                                            </div>

                                            {linePrinters.map((p) => {
                                                const boundScan = lineScanners.find((s) => s.bound_printer_id === p.id);

                                                return (
                                                    <div key={p.id} className="p-3.5 bg-slate-50 rounded-xl border border-slate-200 flex flex-col gap-2">
                                                        <div className="flex justify-between items-start">
                                                            <div>
                                                                <div className="text-xs font-bold text-slate-900 flex items-center gap-1.5 flex-wrap">
                                                                    <Icons.Printer />
                                                                    <span>{p.name}</span>
                                                                    <RoleBadge role={p.printer_role} />
                                                                </div>
                                                                <div className="text-[10px] text-slate-400 font-mono mt-0.5">
                                                                    {p.ip}:{p.port} • Драйвер: {p.driver} • Буфер: {p.buffer_size}
                                                                </div>
                                                            </div>
                                                            <ActiveBadge isActive={p.is_active} />
                                                        </div>

                                                        <div className="p-2.5 bg-white rounded-lg border border-purple-100 flex items-center justify-between text-xs">
                                                            <div className="flex items-center gap-2 truncate max-w-[240px]">
                                                                <Icons.Camera />
                                                                <span className="font-bold text-purple-900 truncate">
                                  {boundScan ? boundScan.name : 'Камера ТЗ не привязана'}
                                </span>
                                                            </div>
                                                            {boundScan ? (
                                                                <span className="text-[10px] font-mono px-2 py-0.5 bg-purple-50 text-purple-700 border border-purple-200 rounded font-bold">
                                  Привязана к #{p.id}
                                </span>
                                                            ) : (
                                                                <span className="text-[10px] text-slate-400 italic">Свободная камера</span>
                                                            )}
                                                        </div>
                                                    </div>
                                                );
                                            })}
                                        </div>
                                    </div>
                                );
                            })}
                        </div>
                    </section>
                )}

                {/* ================================================================= */}
                {/* ВКЛАДКА 3: ДИАГНОСТИКА ЯДРА И ТЕЛЕМЕТРИЯ СКОРОСТЕЙ                 */}
                {/* ================================================================= */}
                {activeTab === 'diag' && (
                    <section className="flex flex-col gap-6">
                        <div className="bg-white p-4 rounded-xl border border-slate-200 shadow-sm flex flex-wrap items-center justify-between gap-4">
                            <div>
                                <h2 className="text-base font-bold text-slate-900">Диагностика сетевого ядра и сокетов</h2>
                                <p className="text-xs text-slate-500">
                                    Телеметрия скорости маркировки, опрос одометров и аудит журнала событий
                                </p>
                            </div>
                            <button
                                onClick={handleOpenLogsAiAudit}
                                className="px-3.5 py-1.5 bg-gradient-to-r from-blue-600 to-indigo-600 hover:from-blue-500 hover:to-indigo-500 text-white rounded-xl text-xs font-bold flex items-center gap-2 shadow transition-all"
                            >
                                <Icons.Sparkles /> AI Экспертиза логов ядра
                            </button>
                        </div>

                        <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
                            <div className="lg:col-span-2 bg-white p-5 rounded-2xl border border-slate-200 shadow-sm flex flex-col gap-4">
                                <div className="flex justify-between items-center border-b border-slate-100 pb-3">
                                    <h3 className="font-bold text-sm text-slate-900 flex items-center gap-2">
                                        <Icons.Chart /> Скорость маркировки vs Верификация камер (DataMatrix/sec)
                                    </h3>
                                    <span className="text-[10px] font-mono text-slate-400">Интервал: 1s</span>
                                </div>

                                <TelemetryChart
                                    data={[
                                        { time: '21:00', prints: 24, scans: 24 },
                                        { time: '21:01', prints: 32, scans: 31 },
                                        { time: '21:02', prints: 38, scans: 38 },
                                        { time: '21:03', prints: 40, scans: 39 },
                                        { time: '21:04', prints: 42, scans: 41 }
                                    ]}
                                />
                            </div>

                            <div className="bg-white p-5 rounded-2xl border border-slate-200 shadow-sm flex flex-col gap-4">
                                <h3 className="font-bold text-sm text-slate-900 border-b border-slate-100 pb-3">
                                    Аппаратные метрики ядра
                                </h3>
                                <div className="grid grid-cols-1 gap-2.5 font-mono text-xs">
                                    <div className="p-3 bg-slate-50 rounded-xl border border-slate-200 flex justify-between items-center">
                                        <span className="text-slate-500 text-[11px]">PING СОКЕТА 9100:</span>
                                        <b className="text-blue-600 text-sm">1.8 ms</b>
                                    </div>
                                    <div className="p-3 bg-slate-50 rounded-xl border border-slate-200 flex justify-between items-center">
                                        <span className="text-slate-500 text-[11px]">АКТИВНЫЕ GOROUTINES:</span>
                                        <b className="text-emerald-600 text-sm">34 потока</b>
                                    </div>
                                    <div className="p-3 bg-slate-50 rounded-xl border border-slate-200 flex justify-between items-center">
                                        <span className="text-slate-500 text-[11px]">БАЗА SQLite MASTER:</span>
                                        <b className="text-purple-600 text-sm">WAL Mode (Active)</b>
                                    </div>
                                    <div className="p-3 bg-slate-50 rounded-xl border border-slate-200 flex justify-between items-center">
                                        <span className="text-slate-500 text-[11px]">СВЯЗОК В РАБОТЕ:</span>
                                        <b className="text-slate-900 text-sm">{scanners.filter(s => s.is_active).length} пар</b>
                                    </div>
                                </div>
                            </div>
                        </div>
                    </section>
                )}
            </main>

            {/* ================================================================= */}
            {/* ФУТЕР: ЖИВАЯ ЛЕНТА СОБЫТИЙ SQLITE STREAM                           */}
            {/* ================================================================= */}
            <footer className="mt-auto bg-slate-900 text-slate-300 p-4 border-t border-slate-800">
                <div className="max-w-[1920px] mx-auto flex flex-col gap-2.5">
                    <div className="flex items-center justify-between text-xs">
                        <div className="flex items-center gap-2 font-bold font-mono uppercase tracking-wider text-slate-400">
                            <Icons.Terminal />
                            <span>SQLite Event Stream (Live Audit)</span>
                        </div>
                        <button
                            onClick={handleOpenLogsAiAudit}
                            className="px-2.5 py-1 bg-slate-800 hover:bg-slate-700 text-indigo-300 border border-slate-700 rounded-lg text-xs font-bold flex items-center gap-1.5 transition-colors"
                        >
                            <Icons.Sparkles /> AI Экспертиза инцидентов
                        </button>
                    </div>

                    <div className="bg-slate-950 rounded-xl border border-slate-800 p-3 h-28 overflow-y-auto font-mono text-xs space-y-1 shadow-inner">
                        {logs.map((l, idx) => (
                            <div key={l.id || idx} className="flex items-start gap-2.5 hover:bg-slate-900/60 px-1.5 py-0.5 rounded transition-colors">
                                <span className="text-slate-500 text-[10px] shrink-0">[{l.time}]</span>
                                <span className="text-blue-400 font-bold shrink-0 min-w-[160px] truncate">
                  {l.printer} {l.line ? `[${l.line}]` : ''}
                </span>
                                <span className={`break-all ${l.type === 'error' ? 'text-rose-400 font-bold' : l.type === 'success' ? 'text-emerald-400' : 'text-slate-300'}`}>
                  {l.message}
                </span>
                            </div>
                        ))}
                    </div>
                </div>
            </footer>

            {/* Модальное окно останова партии по PIN */}
            <PinStopModal
                taskId={stopModalTaskId}
                isOpen={stopModalTaskId !== null}
                onClose={() => setStopModalTaskId(null)}
                onSuccess={(res) => {
                    showToast(
                        'Партия остановлена',
                        `Подтверждено: ${res.total_confirmed} шт, возвращено в очередь: ${res.buffer_reverted_total} шт`,
                        'success'
                    );
                    fetchData();
                }}
                onError={(msg) => showToast('Ошибка останова', msg, 'error')}
            />

            {/* Модальное окно просмотра последних N отсканированных кодов */}
            {scanHistoryModalLine && (
                <div className="fixed inset-0 z-50 bg-slate-950/70 backdrop-blur-sm flex items-center justify-center p-4">
                    <div className="bg-white rounded-2xl border border-slate-200 max-w-4xl w-full shadow-2xl flex flex-col max-h-[90vh] overflow-hidden animate-in fade-in zoom-in-95">
                        <div className="p-4 border-b border-slate-100 flex items-center justify-between bg-gradient-to-r from-slate-900 to-purple-950 text-white">
                            <div className="flex items-center gap-2.5">
                                <div className="p-2 rounded-xl bg-purple-500/20 text-purple-300 border border-purple-400/30">
                                    <Icons.Barcode />
                                </div>
                                <div>
                                    <h3 className="font-bold text-sm tracking-wide flex items-center gap-2">
                                        Журнал последних кодов: {scanHistoryModalLine.name}
                                    </h3>
                                    <p className="text-[11px] text-slate-300 font-mono mt-0.5">
                                        Партия #{scanHistoryModalLine.current_task?.task_id || '—'} • {scanHistoryModalLine.current_task?.template_name || 'Прямой поток'}
                                    </p>
                                </div>
                            </div>
                            <button
                                onClick={() => setScanHistoryModalLine(null)}
                                className="text-slate-400 hover:text-white text-lg font-bold p-1 leading-none"
                            >
                                &times;
                            </button>
                        </div>

                        <div className="p-3 bg-slate-50 border-b border-slate-200 flex flex-wrap items-center justify-between gap-3 text-xs">
                            <div className="flex items-center gap-2">
                                <span className="text-[11px] font-bold text-slate-500 uppercase tracking-wider">Выводить кодов:</span>
                                <div className="flex items-center gap-1 bg-white p-0.5 rounded-lg border border-slate-200">
                                    {[10, 20, 50, 100].map((num) => (
                                        <button
                                            key={num}
                                            onClick={() => handleOpenScanHistoryModal(scanHistoryModalLine, num)}
                                            className={`px-2.5 py-1 rounded text-xs font-mono font-bold transition-all ${
                                                scanHistoryLimit === num
                                                    ? 'bg-purple-600 text-white shadow-2xs'
                                                    : 'text-slate-600 hover:text-slate-900 hover:bg-slate-100'
                                            }`}
                                        >
                                            {num}
                                        </button>
                                    ))}
                                </div>
                            </div>

                            <div className="flex items-center gap-3">
                                <div className="flex items-center gap-2 text-[11px] font-mono">
                  <span className="flex items-center gap-1 text-emerald-700 font-bold">
                    <span className="w-2 h-2 rounded-full bg-emerald-500"></span>
                    MATCHED: {scanHistoryList.filter(s => s.match_status === 'matched').length}
                  </span>
                                    <span className="flex items-center gap-1 text-rose-600 font-bold">
                    <span className="w-2 h-2 rounded-full bg-rose-500"></span>
                    REJECTS: {scanHistoryList.filter(s => s.match_status !== 'matched').length}
                  </span>
                                </div>

                                <button
                                    onClick={() => handleOpenScanHistoryModal(scanHistoryModalLine, scanHistoryLimit)}
                                    disabled={scanHistoryLoading}
                                    className="px-3 py-1 bg-white hover:bg-slate-100 border border-slate-200 text-slate-700 font-bold rounded-lg text-xs flex items-center gap-1.5 transition-colors"
                                >
                                    <Icons.Refresh /> {scanHistoryLoading ? 'Чтение...' : 'Обновить'}
                                </button>
                            </div>
                        </div>

                        <div className="p-4 overflow-y-auto flex-1 bg-white font-mono text-xs">
                            {scanHistoryLoading ? (
                                <div className="py-16 flex flex-col items-center justify-center gap-2 text-slate-400">
                                    <div className="w-6 h-6 border-2 border-purple-600 border-t-transparent rounded-full animate-spin"></div>
                                    <span>Вычитка последних {scanHistoryLimit} кодов из SQLite...</span>
                                </div>
                            ) : scanHistoryList.length === 0 ? (
                                <div className="py-16 text-center text-slate-400 italic">
                                    Нет зарегистрированных сканов по выбранной линии
                                </div>
                            ) : (
                                <div className="divide-y divide-slate-100 border border-slate-200 rounded-xl overflow-hidden shadow-inner">
                                    <div className="bg-slate-50 px-3 py-2 text-[10px] text-slate-500 font-bold uppercase tracking-wider grid grid-cols-12 gap-2">
                                        <div className="col-span-1">Время</div>
                                        <div className="col-span-2">Камера</div>
                                        <div className="col-span-6">DataMatrix GS1 / Код маркировки</div>
                                        <div className="col-span-2 text-center">Статус</div>
                                        <div className="col-span-1 text-right">Копия</div>
                                    </div>

                                    {scanHistoryList.map((item, idx) => {
                                        const isReject = item.match_status !== 'matched';
                                        return (
                                            <div
                                                key={item.id || idx}
                                                className={`px-3 py-2 text-[11px] grid grid-cols-12 gap-2 items-center hover:bg-slate-50/80 transition-colors ${
                                                    isReject ? 'bg-rose-50/40 text-rose-900' : 'text-slate-800'
                                                }`}
                                            >
                                                <div className="col-span-1 text-slate-400 text-[10px]">{item.read_at}</div>
                                                <div className="col-span-2 text-slate-600 text-[10px] truncate" title={item.scanner_name}>
                                                    {item.scanner_name}
                                                </div>
                                                <div className="col-span-6 font-semibold break-all select-all flex items-center gap-1.5">
                          <span className={isReject ? 'text-rose-600 font-bold' : 'text-slate-900'}>
                            {item.code}
                          </span>
                                                    {item.weight && (
                                                        <span className="text-[10px] px-1.5 py-0.2 bg-emerald-100 text-emerald-800 rounded font-bold shrink-0">
                              {item.weight}
                            </span>
                                                    )}
                                                </div>
                                                <div className="col-span-2 text-center">
                          <span
                              className={`px-2 py-0.5 rounded text-[10px] font-bold tracking-wider uppercase ${
                                  isReject
                                      ? 'bg-rose-100 text-rose-700 border border-rose-200'
                                      : 'bg-emerald-100 text-emerald-700 border border-emerald-200'
                              }`}
                          >
                            {isReject ? 'REJECT' : 'MATCHED'}
                          </span>
                                                </div>
                                                <div className="col-span-1 text-right">
                                                    <button
                                                        onClick={() => copyToClipboard(item.code, `modal_${item.id || idx}`)}
                                                        className="p-1 text-slate-400 hover:text-slate-700 hover:bg-slate-100 rounded transition-colors inline-block"
                                                        title="Скопировать полный код"
                                                    >
                                                        {copiedCodeId === `modal_${item.id || idx}` ? <Icons.Check /> : <Icons.Copy />}
                                                    </button>
                                                </div>
                                            </div>
                                        );
                                    })}
                                </div>
                            )}
                        </div>

                        <div className="p-3 border-t border-slate-200 bg-slate-50 flex items-center justify-between text-xs">
              <span className="text-slate-400 font-mono text-[11px]">
                Всего отображено: {scanHistoryList.length} кодов
              </span>
                            <button
                                onClick={() => setScanHistoryModalLine(null)}
                                className="px-4 py-2 bg-slate-200 hover:bg-slate-300 text-slate-800 font-bold rounded-xl text-xs transition-colors"
                            >
                                Закрыть
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Модальное окно полного просмотра всех связок многоголовочной линии */}
            {viewAllModalLine && (
                <div className="fixed inset-0 z-50 bg-slate-950/70 backdrop-blur-sm flex items-center justify-center p-4">
                    <div className="bg-white rounded-2xl border border-slate-200 max-w-4xl w-full shadow-2xl flex flex-col max-h-[90vh] overflow-hidden animate-in fade-in zoom-in-95">
                        <div className="p-4 border-b border-slate-100 flex items-center justify-between bg-slate-900 text-white">
                            <div>
                                <h3 className="font-bold text-sm tracking-wide flex items-center gap-2">
                                    <Icons.Layers /> Все устройства линии: {viewAllModalLine.name}
                                </h3>
                                <p className="text-[11px] text-slate-400 mt-0.5">{viewAllModalLine.description}</p>
                            </div>
                            <button
                                onClick={() => setViewAllModalLine(null)}
                                className="text-slate-400 hover:text-white text-lg font-bold p-1 leading-none"
                            >
                                &times;
                            </button>
                        </div>

                        <div className="p-5 overflow-y-auto flex-1 bg-slate-50">
                            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                                {printers.filter(p => viewAllModalLine.printerIds.includes(p.id)).map(p => {
                                    const boundScan = scanners.find(s => s.bound_printer_id === p.id);
                                    return (
                                        <DevicePairCard
                                            key={p.id}
                                            printer={p}
                                            scanner={boundScan}
                                            lineTarget={viewAllModalLine.current_task ? viewAllModalLine.current_task.total : 5000}
                                            onOpenScanHistory={() => handleOpenScanHistoryModal(viewAllModalLine, 20)}
                                            onCopyCode={copyToClipboard}
                                            copiedCodeId={copiedCodeId}
                                        />
                                    );
                                })}
                            </div>
                        </div>

                        <div className="p-3 border-t border-slate-200 bg-white flex justify-end">
                            <button
                                onClick={() => setViewAllModalLine(null)}
                                className="px-4 py-2 bg-slate-100 hover:bg-slate-200 text-slate-700 font-bold rounded-xl text-xs"
                            >
                                Закрыть
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Модальное окно AI Аналитики */}
            {aiModalOpen && (
                <div className="fixed inset-0 z-50 bg-slate-950/75 backdrop-blur-sm flex items-center justify-center p-4">
                    <div className="bg-white rounded-2xl border border-slate-200 max-w-2xl w-full shadow-2xl flex flex-col max-h-[85vh] overflow-hidden animate-in fade-in zoom-in-95">
                        <div className="p-4 border-b border-slate-100 flex items-center justify-between bg-gradient-to-r from-slate-900 to-indigo-950 text-white">
                            <div className="flex items-center gap-2.5">
                                <div className="p-2 rounded-xl bg-indigo-500/20 text-indigo-300 border border-indigo-400/30">
                                    <Icons.Sparkles />
                                </div>
                                <div>
                                    <h3 className="font-bold text-sm tracking-wide">{aiTitle}</h3>
                                    <p className="text-[11px] text-slate-300 font-mono mt-0.5">{aiContextDesc}</p>
                                </div>
                            </div>
                            <button
                                onClick={() => setAiModalOpen(false)}
                                className="text-slate-400 hover:text-white text-lg font-bold leading-none p-1"
                            >
                                &times;
                            </button>
                        </div>

                        <div className="p-3 bg-slate-50 border-b border-slate-200 flex flex-wrap items-center gap-2 text-xs">
                            <span className="text-[10px] font-bold text-slate-400 uppercase tracking-wider">Быстрые сценарии:</span>
                            <button
                                disabled={aiLoading}
                                onClick={() => handleSendCustomAiQuery('Проанализируй риск рассинхрона печати и верификации, и прогноз окончания партии.')}
                                className="px-2.5 py-1 bg-white hover:bg-slate-100 border border-slate-200 rounded-lg text-[11px] text-slate-700 font-medium transition-colors"
                            >
                                ⏱ Баланс и прогноз ETA
                            </button>
                            <button
                                disabled={aiLoading}
                                onClick={() => handleSendCustomAiQuery('Оцени состояние риббона на маркираторах. Хватит ли ленты до конца текущей партии?')}
                                className="px-2.5 py-1 bg-white hover:bg-slate-100 border border-slate-200 rounded-lg text-[11px] text-slate-700 font-medium transition-colors"
                            >
                                🎗 Расчет запаса риббона
                            </button>
                            <button
                                disabled={aiLoading}
                                onClick={() => handleSendCustomAiQuery('Дай список регламентных действий оператору для минимизации брака на камерах.')}
                                className="px-2.5 py-1 bg-white hover:bg-slate-100 border border-slate-200 rounded-lg text-[11px] text-slate-700 font-medium transition-colors"
                            >
                                📋 Инструктаж оператору
                            </button>
                        </div>

                        <div className="p-5 overflow-y-auto flex-1 font-sans text-xs text-slate-700 leading-relaxed space-y-3">
                            {aiLoading ? (
                                <div className="py-12 flex flex-col items-center justify-center gap-3 text-slate-400">
                                    <div className="w-8 h-8 rounded-full border-2 border-indigo-600 border-t-transparent animate-spin"></div>
                                    <span className="font-mono text-xs text-indigo-600 font-bold">
                    Gemini 3 Flash анализирует телеметрию оборудования...
                  </span>
                                    <span className="text-[11px] text-slate-400">Сверка одометров, сокетов и буферов</span>
                                </div>
                            ) : aiResult ? (
                                <div className="whitespace-pre-wrap bg-slate-50 p-4 rounded-xl border border-slate-200 text-slate-800 font-mono text-[11px] leading-relaxed shadow-inner">
                                    {aiResult}
                                </div>
                            ) : (
                                <div className="py-8 text-center text-slate-400 text-xs italic">
                                    Выберите быстрый сценарий выше или задайте вопрос инженеру АСУ ТП
                                </div>
                            )}
                        </div>

                        <div className="p-3 border-t border-slate-200 bg-white flex items-center gap-2">
                            <input
                                type="text"
                                placeholder="Задать вопрос AI-инженеру (например: почему вырос брак на камере 1?)..."
                                value={aiCustomPrompt}
                                onChange={(e) => setAiCustomPrompt(e.target.value)}
                                onKeyDown={(e) => {
                                    if (e.key === 'Enter' && !aiLoading) handleSendCustomAiQuery();
                                }}
                                disabled={aiLoading}
                                className="flex-1 px-3 py-2 border border-slate-300 rounded-xl text-xs outline-none focus:border-indigo-600 focus:ring-1 focus:ring-indigo-600"
                            />
                            <button
                                onClick={() => handleSendCustomAiQuery()}
                                disabled={aiLoading || !aiCustomPrompt.trim()}
                                className="px-4 py-2 bg-indigo-600 hover:bg-indigo-500 disabled:opacity-50 text-white font-bold rounded-xl text-xs shadow flex items-center gap-1.5 transition-colors"
                            >
                                <Icons.Sparkles /> Спросить
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Toast-уведомления */}
            <NotificationToast toast={toast} onClose={() => setToast(null)} />
        </div>
    );
}