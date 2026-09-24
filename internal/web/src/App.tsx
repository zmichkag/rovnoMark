import { useState } from 'react';
import { useGatewayData, useStopTask } from './hooks/useGateway';
import { LineCard } from './components/LineCard';
import { LogStream } from './components/LogStream';
import { PinStopModal } from './components/PinStopModal';
import { Activity, AlertTriangle, Layers } from 'lucide-react';

export default function App() {
    const { live, topology, logs, isLoading, isError, error } = useGatewayData();
    const stopMutation = useStopTask();
    const [stopTaskId, setStopTaskId] = useState<number | null>(null);

    if (isLoading && !live) {
        return (
            <div className="min-h-screen bg-slate-950 flex items-center justify-center text-slate-500 font-mono text-sm">
                Инициализация соединения с ядром RovnoMark...
            </div>
        );
    }

    if (isError && !live) {
        return (
            <div className="min-h-screen bg-slate-950 flex flex-col items-center justify-center text-rose-400 font-mono gap-2">
                <AlertTriangle className="w-8 h-8" />
                <div className="text-base font-bold">Сбой подключения к API шлюза</div>
                <div className="text-xs text-slate-500">{(error as Error)?.message}</div>
            </div>
        );
    }

    const summary = live?.summary;

    // Извлекаем плоский список сканеров из структуры топологии
    const scanners = (topology?.lines || []).flatMap((l) => l.scanners || []);

    return (
        <div className="min-h-screen bg-slate-950 text-slate-300 font-sans flex flex-col">
            {/* Шапка KPI */}
            <header className="sticky top-0 z-40 bg-slate-900 border-b border-slate-800 shadow-md">
                <div className="max-w-[1920px] mx-auto px-6 py-3 flex items-center justify-between">
                    <div className="flex items-center gap-3">
                        <div className="bg-blue-600 p-2 rounded-xl text-white shadow-lg shadow-blue-900/30">
                            <Activity className="w-5 h-5" />
                        </div>
                        <div>
                            <h1 className="text-base font-black text-white leading-none tracking-wide">RovnoMark</h1>
                            <span className="text-[10px] text-slate-400 font-mono uppercase tracking-widest">Gateway Control v2.5</span>
                        </div>
                    </div>

                    <div className="flex items-center gap-4">
                        <div className="flex items-center gap-2 bg-slate-950/60 px-3.5 py-1.5 rounded-lg border border-slate-800 text-xs">
                            <Layers className="w-4 h-4 text-emerald-400" />
                            <span>Линий в работе: <b className="text-white font-mono">{summary?.active_tasks ?? 0}</b> / {summary?.total_lines ?? 0}</span>
                        </div>

                        <div className={`flex items-center gap-2 px-3.5 py-1.5 rounded-lg border text-xs ${
                            (summary?.printers_offline ?? 0) > 0 ? 'bg-rose-500/10 border-rose-500/30 text-rose-400' : 'bg-slate-950/60 border-slate-800 text-slate-400'
                        }`}>
                            <AlertTriangle className="w-4 h-4" />
                            <span>Оффлайн: <b className="font-mono">{summary?.printers_offline ?? 0}</b></span>
                        </div>
                    </div>
                </div>
            </header>

            {/* Сетка линий */}
            <main className="p-6 max-w-[1920px] mx-auto w-full flex-grow">
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-6">
                    {live?.lines.map((line) => (
                        <LineCard
                            key={line.line_id}
                            line={line}
                            printerCatalog={live.printers}
                            scanners={scanners}
                            onStopClick={(taskId) => setStopTaskId(taskId)}
                        />
                    ))}
                </div>
            </main>

            {/* Футер системных логов */}
            <LogStream logs={logs} />

            {/* Модалка PIN-останова */}
            <PinStopModal
                taskId={stopTaskId}
                isOpen={stopTaskId !== null}
                onClose={() => setStopTaskId(null)}
                isPending={stopMutation.isPending}
                onConfirm={(taskId) => {
                    stopMutation.mutate(taskId, {
                        onSuccess: () => setStopTaskId(null),
                    });
                }}
            />
        </div>
    );
}