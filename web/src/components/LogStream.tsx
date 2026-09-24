import React from 'react';
import { EventLogItem } from '../api/types';
import { Terminal } from 'lucide-react';

export const LogStream: React.FC<{ logs: EventLogItem[] }> = ({ logs }) => {
    return (
        <footer className="mt-auto border-t border-slate-800 bg-[#0a0d14] p-3 shrink-0">
            <div className="max-w-[1920px] mx-auto flex flex-col gap-1.5 h-32">
                <div className="flex items-center justify-between text-[11px] text-slate-500 font-bold uppercase tracking-wider px-1">
          <span className="flex items-center gap-1.5">
            <Terminal className="w-3.5 h-3.5 text-blue-400" />
            Live Event Stream (SQLite Master DB)
          </span>
                    <span className="text-[10px] font-mono">SQLite WAL Mode Active</span>
                </div>

                <div className="bg-slate-950 rounded-xl border border-slate-800/80 p-2.5 overflow-y-auto font-mono text-xs space-y-1 shadow-inner flex-grow">
                    {logs.length > 0 ? (
                        logs.map((log) => {
                            const timeStr = log.timestamp ? new Date(log.timestamp).toLocaleTimeString('ru-RU') : '—';
                            const isError = log.event_type === 'error';
                            const isWarn = log.event_type === 'warn' || log.event_type === 'warning';

                            return (
                                <div key={log.id} className="flex items-start gap-2.5 text-[11px] py-0.5 hover:bg-slate-900/50 px-1.5 rounded">
                                    <span className="text-slate-600 shrink-0">[{timeStr}]</span>
                                    <span className="text-slate-300 font-semibold shrink-0 min-w-[140px] truncate">
                    {log.printer_name} {log.line_name && `[${log.line_name}]`}
                  </span>
                                    <span className={`break-all ${
                                        isError ? 'text-rose-400 font-bold' : isWarn ? 'text-amber-400' : 'text-slate-400'
                                    }`}>
                    {log.message}
                  </span>
                                </div>
                            );
                        })
                    ) : (
                        <div className="text-slate-600 italic">Событий не зафиксировано...</div>
                    )}
                </div>
            </div>
        </footer>
    );
};