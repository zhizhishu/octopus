'use client';

import { useState } from 'react';
import { Loader2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useQueryClient } from '@tanstack/react-query';
import { useRedeemCode } from '@/api/endpoints/redeem';
import { useAuthStore } from '@/api/endpoints/user';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { toast } from '@/components/common/Toast';
import { btnPrimary } from '@/components/modules/user/components/ui-bits';
import { inputCls } from './bits';

// 普通用户兑换管理员发的码（余额或月卡）。原来是页面上的一块面板，收进按钮打开的小弹窗。
export function RedeemCodeDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
    const t = useTranslations('keys.redeem');
    const [code, setCode] = useState('');
    const redeem = useRedeemCode();
    const queryClient = useQueryClient();
    const submit = () => {
        const value = code.trim();
        if (!value) return;
        redeem.mutate(value, {
            onSuccess: (user) => {
                useAuthStore.setState({ user });
                queryClient.setQueryData(['user', 'status'], user);
                setCode('');
                toast.success(t('done'));
                onOpenChange(false);
            },
            onError: (error) => toast.error(t('failed'), { description: (error as { message?: string } | null)?.message }),
        });
    };
    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="sm:max-w-[420px]">
                <DialogHeader>
                    <DialogTitle>{t('title')}</DialogTitle>
                    <DialogDescription>{t('desc')}</DialogDescription>
                </DialogHeader>
                <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); submit(); }}>
                    <input autoFocus className={inputCls} value={code} onChange={(e) => setCode(e.target.value)} placeholder={t('placeholder')} />
                    <button type="submit" className={btnPrimary} disabled={redeem.isPending || !code.trim()}>
                        {redeem.isPending ? <Loader2 className="size-4 animate-spin" /> : t('submit')}
                    </button>
                </form>
            </DialogContent>
        </Dialog>
    );
}
