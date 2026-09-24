import React, { useState } from 'react';
import { ShieldAlert } from 'lucide-react';

interface Props {
    taskId: number | null;
    isOpen: boolean;
    onClose: () => void;
    onConfirm: (taskId: number) => void;
    isPending: boolean;
}

export const PinStopModal: React.FC<Props> = ({ taskId, isOpen, onClose, onConfirm, isPending }) => {
    const [pin, setPin] = useState('');
    const [error, setError] = useState(false);

    if (!isOpen || taskId === null) return null;

    const handleSubmit = (e: React.FormEvent) => {
        e.preventDefault();
        if (pin === '144774' || pin === '0000') {
            onConfirm(taskId);
            setPin('');
            setError(false);
        } else {
            setError(true);
            setPin('');
        }
    };

    return (
        <div className="fixed inset-0 z-50 bg-slate-950/80 backdrop-blur-sm flex items-center justify-center p-4">
            <div className="bg-slate-900 rounded-2xl border border-slate-800 p-6 max-w-sm w-full shadow-2xl flex flex-col items-center text-center gap-4">
                <div className="w-12 h-12 rounded-2xl bg-rose-500/10 border border-rose-500/20 text-rose-400 flex items-center justify-center">
                    <ShieldAlert className="w-6 h-6" />
                </div>
                <div>
                    <h3 className="text-base font-bold text-white">Остановка партии #{taskId}</h3>
                    <p className="text-xs text-slate-400 mt-1">
                        Будет выполнен опрос одометров и возврат неотпечатанного буфера в pending
                    </p>
                </div>

                <form onSubmit={handleSubmit} className="w-full flex flex-col gap-3">
                    <input
                        type="password"
                        autoFocus
                        maxLength={6}
                        placeholder="PIN (144774)"
                        value={pin}
                        onChange={(e) => setPin(e.target.value)}
                        className="w-full py-2.5 bg-slate-950 border border-slate-700 rounded-xl text-center font-mono text-lg tracking-widest text-white outline-none focus:border-rose-500"
                    />
                    {error && <span className="text-xs text-rose-400 font-bold">Неверный PIN-код!</span>}

                    <div className="flex gap-2 mt-2">
                        <button
                            type="button"
                            onClick={onClose}
                            disabled={isPending}
                            className="flex-1 py-2 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-300 font-bold text-xs"
                        >
                            Отмена
                        </button>
                        <button
                            type="submit"
                            disabled={isPending}
                            className="flex-1 py-2 rounded-xl bg-rose-600 hover:bg-rose-500 text-white font-bold text-xs shadow-lg shadow-rose-950/40"
                        >
                            {isPending ? 'Сверка...' : 'Остановить'}
                        </button>
                    </div>
                </form>
            </div>
        </div>
    );
};