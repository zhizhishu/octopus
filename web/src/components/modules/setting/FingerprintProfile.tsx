'use client';

import { useState } from 'react';
import { useTranslations } from 'next-intl';
import { Check, Fingerprint, Loader2, Pencil, Plus, Trash2, X } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle,
} from '@/components/ui/dialog';
import {
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
} from '@/components/ui/select';
import {
    useFingerprintProfileList,
    useCreateFingerprintProfile,
    useUpdateFingerprintProfile,
    useDeleteFingerprintProfile,
    type FingerprintProfile,
} from '@/api/endpoints/fingerprint-profile';
import { toast } from '@/components/common/Toast';

// claude_stabilize 三态：'follow' = 跟随全局（null），'on'/'off' = 显式布尔。
type StabilizeChoice = 'follow' | 'on' | 'off';

type ProfileDraft = {
    name: string;
    seed: string;
    claudeUserAgent: string;
    claudePackageVersion: string;
    claudeRuntimeVersion: string;
    claudeOS: string;
    claudeArch: string;
    claudeTimeout: string;
    claudeStabilize: StabilizeChoice;
    codexUserAgent: string;
    codexOriginator: string;
    codexBetaFeatures: string;
    genericUA: string;
};

const emptyDraft: ProfileDraft = {
    name: '',
    seed: '',
    claudeUserAgent: '',
    claudePackageVersion: '',
    claudeRuntimeVersion: '',
    claudeOS: '',
    claudeArch: '',
    claudeTimeout: '',
    claudeStabilize: 'follow',
    codexUserAgent: '',
    codexOriginator: '',
    codexBetaFeatures: '',
    genericUA: '',
};

function stabilizeToChoice(value: boolean | null): StabilizeChoice {
    if (value === null || value === undefined) return 'follow';
    return value ? 'on' : 'off';
}

function choiceToStabilize(choice: StabilizeChoice): boolean | null {
    if (choice === 'follow') return null;
    return choice === 'on';
}

function profileToDraft(profile: FingerprintProfile): ProfileDraft {
    return {
        name: profile.name,
        seed: profile.seed,
        claudeUserAgent: profile.claude_user_agent,
        claudePackageVersion: profile.claude_package_version,
        claudeRuntimeVersion: profile.claude_runtime_version,
        claudeOS: profile.claude_os,
        claudeArch: profile.claude_arch,
        claudeTimeout: profile.claude_timeout,
        claudeStabilize: stabilizeToChoice(profile.claude_stabilize),
        codexUserAgent: profile.codex_user_agent,
        codexOriginator: profile.codex_originator,
        codexBetaFeatures: profile.codex_beta_features,
        genericUA: profile.generic_ua,
    };
}

export function SettingFingerprintProfile() {
    const t = useTranslations('setting');
    const { data: profiles } = useFingerprintProfileList();
    const createProfile = useCreateFingerprintProfile();
    const updateProfile = useUpdateFingerprintProfile();
    const deleteProfile = useDeleteFingerprintProfile();

    // editing === 'new' 表示新建；number 表示编辑该 id；null 表示对话框关闭。
    const [editing, setEditing] = useState<'new' | number | null>(null);
    const [draft, setDraft] = useState<ProfileDraft>(emptyDraft);
    const [confirmingDeleteID, setConfirmingDeleteID] = useState<number | null>(null);

    const list = [...(profiles ?? [])].sort((a, b) => a.id - b.id);
    const isSaving = createProfile.isPending || updateProfile.isPending;

    const openCreate = () => {
        setDraft(emptyDraft);
        setEditing('new');
    };

    const openEdit = (profile: FingerprintProfile) => {
        setDraft(profileToDraft(profile));
        setEditing(profile.id);
    };

    const updateDraft = <K extends keyof ProfileDraft>(key: K, value: ProfileDraft[K]) => {
        setDraft((prev) => ({ ...prev, [key]: value }));
    };

    const handleSave = () => {
        const name = draft.name.trim();
        if (!name) {
            toast.error(t('fingerprintProfile.nameRequired'));
            return;
        }
        const payload = {
            name,
            seed: draft.seed.trim(),
            claude_user_agent: draft.claudeUserAgent.trim(),
            claude_package_version: draft.claudePackageVersion.trim(),
            claude_runtime_version: draft.claudeRuntimeVersion.trim(),
            claude_os: draft.claudeOS.trim(),
            claude_arch: draft.claudeArch.trim(),
            claude_timeout: draft.claudeTimeout.trim(),
            claude_stabilize: choiceToStabilize(draft.claudeStabilize),
            codex_user_agent: draft.codexUserAgent.trim(),
            codex_originator: draft.codexOriginator.trim(),
            codex_beta_features: draft.codexBetaFeatures.trim(),
            generic_ua: draft.genericUA.trim(),
        };

        if (editing === 'new') {
            createProfile.mutate(payload, {
                onSuccess: () => {
                    toast.success(t('saved'));
                    setEditing(null);
                },
                onError: (error) => toast.error(error instanceof Error ? error.message : String(error)),
            });
        } else if (typeof editing === 'number') {
            updateProfile.mutate({ id: editing, ...payload }, {
                onSuccess: () => {
                    toast.success(t('saved'));
                    setEditing(null);
                },
                onError: (error) => toast.error(error instanceof Error ? error.message : String(error)),
            });
        }
    };

    const handleDelete = (id: number) => {
        if (confirmingDeleteID !== id) {
            setConfirmingDeleteID(id);
            return;
        }
        deleteProfile.mutate(id, {
            onSuccess: () => {
                toast.success(t('saved'));
                setConfirmingDeleteID(null);
            },
            onError: (error) => toast.error(error instanceof Error ? error.message : String(error)),
        });
    };

    return (
        <div className="min-w-0 space-y-5 rounded-3xl border border-border bg-card p-4 sm:p-6">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0 space-y-1">
                    <h2 className="flex min-w-0 items-center gap-2 text-lg font-bold text-card-foreground">
                        <Fingerprint className="h-5 w-5 shrink-0" />
                        {t('fingerprintProfile.title')}
                    </h2>
                    <p className="text-xs leading-5 text-muted-foreground">{t('fingerprintProfile.description')}</p>
                </div>
                <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={openCreate}
                    className="h-8 shrink-0 rounded-xl"
                >
                    <Plus className="size-3.5" />
                    {t('fingerprintProfile.add')}
                </Button>
            </div>

            {list.length === 0 ? (
                <div className="rounded-2xl border border-dashed border-border/70 bg-muted/20 p-6 text-center text-sm text-muted-foreground">
                    {t('fingerprintProfile.empty')}
                </div>
            ) : (
                <div className="grid gap-2.5">
                    {list.map((profile, index) => (
                        <div
                            key={profile.id}
                            className="grid min-w-0 gap-2 rounded-2xl border border-border/70 bg-muted/20 p-3 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center"
                        >
                            <div className="min-w-0 space-y-1">
                                <div className="flex flex-wrap items-center gap-2">
                                    <span className="truncate text-sm font-semibold text-card-foreground">{profile.name}</span>
                                    <span className="rounded-full bg-background px-2 py-0.5 text-[11px] text-muted-foreground">#{index + 1}</span>
                                </div>
                                <div className="grid gap-0.5 text-[11px] leading-5 text-muted-foreground">
                                    <div className="min-w-0 truncate">
                                        <span className="font-mono">{t('fingerprintProfile.fields.claudeUserAgent')}</span>
                                        {': '}
                                        {profile.claude_user_agent || t('fingerprintProfile.followGlobal')}
                                    </div>
                                    <div className="min-w-0 truncate">
                                        <span className="font-mono">{t('fingerprintProfile.fields.claudeOS')}</span>
                                        {': '}
                                        {profile.claude_os || t('fingerprintProfile.followGlobal')}
                                        {' / '}
                                        {profile.claude_arch || t('fingerprintProfile.followGlobal')}
                                    </div>
                                </div>
                            </div>
                            <div className="flex shrink-0 items-center gap-1.5 justify-self-end">
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    onClick={() => openEdit(profile)}
                                    className="h-8 rounded-xl"
                                >
                                    <Pencil className="size-3.5" />
                                    {t('fingerprintProfile.edit')}
                                </Button>
                                <Button
                                    type="button"
                                    variant={confirmingDeleteID === profile.id ? 'destructive' : 'ghost'}
                                    size="sm"
                                    onClick={() => handleDelete(profile.id)}
                                    onMouseLeave={() => confirmingDeleteID === profile.id && setConfirmingDeleteID(null)}
                                    disabled={deleteProfile.isPending}
                                    className="h-8 rounded-xl text-muted-foreground hover:text-destructive"
                                >
                                    <Trash2 className="size-3.5" />
                                    {confirmingDeleteID === profile.id ? t('fingerprintProfile.confirmDelete') : t('fingerprintProfile.delete')}
                                </Button>
                            </div>
                        </div>
                    ))}
                </div>
            )}

            <Dialog open={editing !== null} onOpenChange={(open) => { if (!open) setEditing(null); }}>
                <DialogContent className="max-h-[calc(100dvh-1rem)] w-[calc(100vw-1rem)] max-w-[calc(100vw-1rem)] overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-2xl">
                    <DialogHeader className="border-b border-border/60 px-5 py-4 text-left">
                        <DialogTitle className="flex items-center gap-2 text-lg">
                            <Fingerprint className="size-5" />
                            {editing === 'new' ? t('fingerprintProfile.createTitle') : t('fingerprintProfile.editTitle')}
                        </DialogTitle>
                        <DialogDescription className="text-sm">
                            {t('fingerprintProfile.dialogHint')}
                        </DialogDescription>
                    </DialogHeader>
                    <div className="grid max-h-[min(70dvh,34rem)] gap-4 overflow-y-auto px-5 py-4">
                        <div className="grid gap-2">
                            <label htmlFor="fp-name" className="text-sm font-medium text-card-foreground">
                                {t('fingerprintProfile.fields.name')}
                            </label>
                            <Input
                                id="fp-name"
                                value={draft.name}
                                onChange={(event) => updateDraft('name', event.target.value)}
                                placeholder={t('fingerprintProfile.placeholders.name')}
                                className="rounded-xl"
                            />
                        </div>

                        <div className="grid gap-2">
                            <label htmlFor="fp-seed" className="text-sm font-medium text-card-foreground">
                                {t('fingerprintProfile.fields.seed')}
                            </label>
                            <Input
                                id="fp-seed"
                                value={draft.seed}
                                onChange={(event) => updateDraft('seed', event.target.value)}
                                placeholder={t('fingerprintProfile.placeholders.seed')}
                                className="rounded-xl font-mono text-xs"
                            />
                        </div>

                        <div className="grid gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                            <div className="text-sm font-semibold text-card-foreground">{t('fingerprintProfile.groups.claude')}</div>
                            <div className="grid gap-2">
                                <label htmlFor="fp-claude-ua" className="text-xs font-medium text-muted-foreground">
                                    {t('fingerprintProfile.fields.claudeUserAgent')}
                                </label>
                                <Input
                                    id="fp-claude-ua"
                                    value={draft.claudeUserAgent}
                                    onChange={(event) => updateDraft('claudeUserAgent', event.target.value)}
                                    placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                    className="rounded-xl font-mono text-xs"
                                />
                            </div>
                            <div className="grid gap-2 sm:grid-cols-2">
                                <div className="grid gap-2">
                                    <label htmlFor="fp-claude-package" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.claudePackageVersion')}
                                    </label>
                                    <Input
                                        id="fp-claude-package"
                                        value={draft.claudePackageVersion}
                                        onChange={(event) => updateDraft('claudePackageVersion', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                                <div className="grid gap-2">
                                    <label htmlFor="fp-claude-runtime" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.claudeRuntimeVersion')}
                                    </label>
                                    <Input
                                        id="fp-claude-runtime"
                                        value={draft.claudeRuntimeVersion}
                                        onChange={(event) => updateDraft('claudeRuntimeVersion', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                                <div className="grid gap-2">
                                    <label htmlFor="fp-claude-os" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.claudeOS')}
                                    </label>
                                    <Input
                                        id="fp-claude-os"
                                        value={draft.claudeOS}
                                        onChange={(event) => updateDraft('claudeOS', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                                <div className="grid gap-2">
                                    <label htmlFor="fp-claude-arch" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.claudeArch')}
                                    </label>
                                    <Input
                                        id="fp-claude-arch"
                                        value={draft.claudeArch}
                                        onChange={(event) => updateDraft('claudeArch', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                                <div className="grid gap-2">
                                    <label htmlFor="fp-claude-timeout" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.claudeTimeout')}
                                    </label>
                                    <Input
                                        id="fp-claude-timeout"
                                        value={draft.claudeTimeout}
                                        onChange={(event) => updateDraft('claudeTimeout', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                                <div className="grid gap-2">
                                    <label htmlFor="fp-claude-stabilize" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.claudeStabilize')}
                                    </label>
                                    <Select
                                        value={draft.claudeStabilize}
                                        onValueChange={(value) => updateDraft('claudeStabilize', value as StabilizeChoice)}
                                    >
                                        <SelectTrigger id="fp-claude-stabilize" className="rounded-xl w-full border border-border px-3 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                                            <SelectValue />
                                        </SelectTrigger>
                                        <SelectContent className="rounded-xl">
                                            <SelectItem className="rounded-xl" value="follow">{t('fingerprintProfile.stabilize.follow')}</SelectItem>
                                            <SelectItem className="rounded-xl" value="on">{t('fingerprintProfile.stabilize.on')}</SelectItem>
                                            <SelectItem className="rounded-xl" value="off">{t('fingerprintProfile.stabilize.off')}</SelectItem>
                                        </SelectContent>
                                    </Select>
                                </div>
                            </div>
                        </div>

                        <div className="grid gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                            <div className="text-sm font-semibold text-card-foreground">{t('fingerprintProfile.groups.codex')}</div>
                            <div className="grid gap-2">
                                <label htmlFor="fp-codex-ua" className="text-xs font-medium text-muted-foreground">
                                    {t('fingerprintProfile.fields.codexUserAgent')}
                                </label>
                                <Input
                                    id="fp-codex-ua"
                                    value={draft.codexUserAgent}
                                    onChange={(event) => updateDraft('codexUserAgent', event.target.value)}
                                    placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                    className="rounded-xl font-mono text-xs"
                                />
                            </div>
                            <div className="grid gap-2 sm:grid-cols-2">
                                <div className="grid gap-2">
                                    <label htmlFor="fp-codex-originator" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.codexOriginator')}
                                    </label>
                                    <Input
                                        id="fp-codex-originator"
                                        value={draft.codexOriginator}
                                        onChange={(event) => updateDraft('codexOriginator', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                                <div className="grid gap-2">
                                    <label htmlFor="fp-codex-beta" className="text-xs font-medium text-muted-foreground">
                                        {t('fingerprintProfile.fields.codexBetaFeatures')}
                                    </label>
                                    <Input
                                        id="fp-codex-beta"
                                        value={draft.codexBetaFeatures}
                                        onChange={(event) => updateDraft('codexBetaFeatures', event.target.value)}
                                        placeholder={t('fingerprintProfile.placeholders.followGlobal')}
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                            </div>
                        </div>

                        <div className="grid gap-2 rounded-2xl border border-border/70 bg-muted/20 p-3">
                            <label htmlFor="fp-generic-ua" className="text-sm font-semibold text-card-foreground">
                                {t('fingerprintProfile.fields.genericUA')}
                            </label>
                            <Input
                                id="fp-generic-ua"
                                value={draft.genericUA}
                                onChange={(event) => updateDraft('genericUA', event.target.value)}
                                placeholder={t('fingerprintProfile.placeholders.genericUA')}
                                className="rounded-xl font-mono text-xs"
                            />
                            <p className="text-[11px] leading-5 text-muted-foreground">{t('fingerprintProfile.genericUAHint')}</p>
                        </div>
                    </div>
                    <DialogFooter className="flex-col-reverse gap-2 border-t border-border/60 px-5 py-4 sm:flex-row">
                        <Button
                            type="button"
                            variant="outline"
                            onClick={() => setEditing(null)}
                            className="w-full rounded-xl sm:w-auto"
                        >
                            <X className="size-4" />
                            {t('fingerprintProfile.cancel')}
                        </Button>
                        <Button
                            type="button"
                            onClick={handleSave}
                            disabled={isSaving}
                            className="w-full rounded-xl sm:w-auto"
                        >
                            {isSaving ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
                            {t('fingerprintProfile.save')}
                        </Button>
                    </DialogFooter>
                </DialogContent>
            </Dialog>
        </div>
    );
}
