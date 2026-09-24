import React from 'react';
import { LiveLineItem, LivePrinterItem, ScannerItem } from '../api/types';
import { StopCircle, Server, Camera } from 'lucide-react';

interface Props {
    line: LiveLineItem;
    printerCatalog: Record<string, LivePrinterItem>;
    scanners: ScannerItem[];
    onStopClick: (taskId: number) => void;
}

export const LineCard: React.FC<Props> = ({ line, printerCatalog, scanners, onStopClick }) => {
    const task = line.current_task;
    const isError = line.status.toLowerCase() === 'error';
    const isActive = line.status.toLowerCase() === 'active';

    // Выработка партии
    const printed = task?.stats?.printed ?? task?.printed ?? 0;
    const total = task?.stats?.total ?? task?.total ?? 1;
    const printedPct = Math.min(Math.round((printed / total) * 100), 100);

    // Сканы камер линии
    const lineScanners = scanners.filter((s) => s.line_id === line.line_id);
    const totalScanned = lineScanners.reduce((sum, sc) => sum + (sc.scans_count || 0), 0);
    const scannedPct = Math.min(Math.round((totalScanned / total) * 100), 100);

    return (
        <div className={`flex flex-col bg-slate-900 border rounded-2xl shadow-lg overflow-hidden transition-all ${
            isError ? 'border-rose-900/60 shadow-rose-950/20' : 'border-slate-800'
        }`}>
            {/* Шапка линии */}
            <div className="px-5 py-3.5 flex justify-between items-center border-b border-slate-800 bg-slate-900/80">
                <div>
                    <h2 className="text-base font-bold text-white tracking-tight">{line.line_name}</h2>
                    <p className="text-xs text-slate-400 mt-0.5">{line.description}</p>
                </div>
                <span className={`px-2.5 py-1 rounded-md text-[10px] font-bold tracking-wider uppercase border flex items-center gap-1.5 ${
                    isActive ? 'bg-emerald-500/10 text-emerald-400 border-emerald-500/30' :
                        isError ? 'bg-rose-500/10 text-rose-400 border-rose-500/30' :
                            'bg-slate-800 text-slate-400 border-slate-700'
                }`}>
          {isActive && <span className="w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse" />}
                    {isError && <span className="w-1.5 h-1.5 rounded-full bg-rose-400 animate-ping" />}
                    {line.status}
        </span>
            </div>

            <div className="p-4 flex flex-col gap-4 flex-grow">
                {/* Блок партии с двойным прогресс-баром */}
                {task ? (
                    <div className="bg-slate-950/50 rounded-xl p-3.5 border border-slate-800 flex flex-col gap-3">
                        <div className="flex justify-between items-start">
                            <div>
                <span className="bg-blue-500/10 text-blue-400 text-[10px] px-2 py-0.5 rounded font-mono font-bold border border-blue-500/20">
                  ПАРТИЯ #{task.task_id}
                </span>
                                <h3 className="text-slate-200 font-semibold text-xs mt-1.5 truncate max-w-[220px]" title={task.template_name}>
                                    {task.template_name}
                                </h3>
                            </div>
                            <button
                                onClick={() => onStopClick(task.task_id)}
                                className="p-1.5 rounded-lg bg-slate-800 border border-slate-700 hover:bg-rose-500/20 hover:border-rose-500/40 text-slate-300 hover:text-rose-400 transition-colors"
                                title="Остановить партию со сверкой одометров"
                            >
                                <StopCircle className="w-4 h-4" />
                            </button>
                        </div>

                        <div className="flex flex-col gap-2 pt-1 border-t border-slate-800/80">
                            {/* Прогресс-бар печати */}
                            <div>
                                <div className="flex justify-between text-[11px] mb-1 font-mono">
                  <span className="text-slate-400 font-sans flex items-center gap-1">
                    <Server className="w-3 h-3 text-blue-400" /> Печать принтеров:
                  </span>
                                    <span>
                    <b className="text-white">{printed.toLocaleString()}</b>
                    <span className="text-slate-500"> / {total.toLocaleString()}</span>
                    <span className="text-blue-400 ml-1.5 font-bold">({printedPct}%)</span>
                  </span>
                                </div>
                                <div className="w-full bg-slate-900 rounded-full h-1.5 overflow-hidden border border-slate-800">
                                    <div className="bg-blue-500 h-full rounded-full transition-all duration-500" style={{ width: `${printedPct}%` }} />
                                </div>
                            </div>

                            {/* Прогресс-бар верификации */}
                            <div>
                                <div className="flex justify-between text-[11px] mb-1 font-mono">
                  <span className="text-slate-400 font-sans flex items-center gap-1">
                    <Camera className="w-3 h-3 text-purple-400" /> Верификация камер:
                  </span>
                                    <span>
                    <b className="text-purple-300">{totalScanned.toLocaleString()}</b>
                    <span className="text-slate-500"> / {total.toLocaleString()}</span>
                    <span className="text-purple-400 ml-1.5 font-bold">({scannedPct}%)</span>
                  </span>
                                </div>
                                <div className="w-full bg-slate-900 rounded-full h-1.5 overflow-hidden border border-slate-800">
                                    <div className="bg-purple-500 h-full rounded-full transition-all duration-500" style={{ width: `${scannedPct}%` }} />
                                </div>
                            </div>
                        </div>
                    </div>
                ) : (
                    <div className="bg-slate-950/30 rounded-xl p-4 border border-dashed border-slate-800 text-center flex flex-col items-center justify-center text-slate-500 min-h-[90px]">
                        <span className="text-xs font-medium">Нет активного задания от 1С</span>
                    </div>
                )}

                {/* Связки Принтер ⇄ Камера */}
                <div>
                    <div className="text-[10px] font-bold text-slate-500 uppercase tracking-widest mb-2">
                        Связки оборудования ({line.printers.length})
                    </div>
                    <div className="flex flex-col gap-2.5">
                        {line.printers.map((pId) => {
                            const p = printerCatalog[pId];
                            if (!p) return null;

                            const boundScanner = lineScanners.find((s) => s.target_device_id === p.id);
                            const isOffline = p.status.includes('ОФФЛАЙН') || p.status === 'INITIALIZING';
                            const ribbonVal = p.ribbon ? parseInt(p.ribbon) : null;

                            return (
                                <div key={p.id} className="p-3 rounded-xl bg-slate-950/40 border border-slate-800 flex flex-col gap-2">
                                    <div className="flex justify-between items-start">
                                        <div className="flex items-center gap-2">
                                            <Server className={`w-3.5 h-3.5 ${isOffline ? 'text-rose-400' : 'text-blue-400'}`} />
                                            <div>
                                                <div className="font-bold text-slate-200 text-xs">{p.name}</div>
                                                <div className="text-[10px] font-mono text-slate-500">{p.ip}:{p.port}</div>
                                            </div>
                                        </div>
                                        <div className="flex items-center gap-2 font-mono text-[10px]">
                                            {ribbonVal !== null && (
                                                <span className="text-slate-400">Риббон: <b className={ribbonVal <= 20 ? 'text-rose-400' : 'text-slate-300'}>{ribbonVal}%</b></span>
                                            )}
                                            <span className="text-slate-400">Буфер: <b className="text-indigo-400">{p.queue_free}</b></span>
                                            <span className={`px-1.5 py-0.5 rounded text-[9px] font-bold uppercase ${
                                                isOffline ? 'bg-rose-500/10 text-rose-400' : 'bg-slate-800 text-slate-300'
                                            }`}>
                        {p.status}
                      </span>
                                        </div>
                                    </div>

                                    {/* Привязанная камера */}
                                    {boundScanner ? (
                                        <div className="p-2 rounded-lg bg-purple-950/20 border border-purple-900/30 flex justify-between items-center text-[11px]">
                                            <div className="flex items-center gap-1.5 text-purple-300">
                                                <Camera className="w-3 h-3 text-purple-400" />
                                                <span>{boundScanner.name}</span>
                                            </div>
                                            <div className="font-mono text-[10px] flex items-center gap-3">
                                                <span className="text-slate-400">Сканов: <b className="text-purple-300">{boundScanner.scans_count ?? 0}</b></span>
                                                <span className="text-slate-400">Брак: <b className={(boundScanner.rejects_count ?? 0) > 0 ? 'text-rose-400 font-bold' : 'text-slate-400'}>
                          {boundScanner.rejects_count ?? 0}
                        </b></span>
                                            </div>
                                        </div>
                                    ) : (
                                        <div className="text-[10px] text-slate-600 italic px-2 py-0.5">Камера не привязана</div>
                                    )}
                                </div>
                            );
                        })}
                    </div>
                </div>
            </div>
        </div>
    );
};