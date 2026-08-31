import { useEffect, useRef, useState } from 'react';
import {
    MorphingDialogClose,
    MorphingDialogTitle,
    MorphingDialogDescription,
    useMorphingDialog,
} from '@/components/ui/morphing-dialog';
import { useCreateChannel, ChannelType } from '@/api/endpoints/channel';
import { useFingerprintProfileList } from '@/api/endpoints/fingerprint-profile';
import { useTranslations } from 'next-intl';
import { ChannelForm, normalizeBaseUrlForChannelType, type ChannelFormData } from './Form';
import { ChannelImportCSV } from './ImportCSV';

export function CreateDialogContent() {
    const { setIsOpen } = useMorphingDialog();
    const createChannel = useCreateChannel();
    const { data: fingerprintProfiles } = useFingerprintProfileList();
    // NEW channels default their client-fingerprint profile to the built-in
    // "Linux · Debian" preset. Resolve it by NAME (not a hard-coded id) so it stays
    // correct across backend id migrations; fall back to 0 (跟随全局) whenever the
    // preset isn't loaded/found yet, so we never point the form at a non-existent id.
    const debianProfileId = fingerprintProfiles?.find((p) => p.name === 'Linux · Debian')?.id ?? 0;
    const appliedDebianDefault = useRef(false);
    const [formData, setFormData] = useState<ChannelFormData>({
        name: '',
        type: ChannelType.OpenAIChat,
        priority: 0,
        max_concurrent: 0,
        rpm_limit: 0,
        key_select_strategy: 0,
        disable_circuit_breaker: false,
        race_mode: false,
        race_key_concurrency: 2,
        race_delay_ms: 0,
        base_urls: [{ url: '', delay: 0 }],
        custom_header: [],
        cloak_mode: 'auto',
        cloak_profile_id: 0,
        channel_proxy: '',
        param_override: '',
        system_prompt_override: '',
        prompt_override_mode: 'append_system',
        keys: [{ enabled: true, channel_key: '', remark: '' }],
        model: '',
        custom_model: '',
        discovered_models: [],
        selected_models: [],
        anthropic_context_1m: false,
        thinking_to_content: false,
        auto_sync: false,
        enabled: true,
        proxy: false,
        match_regex: '',
        openai_chat_path: '',
        openai_models_path: '',
    });
    const t = useTranslations('channel.create');

    // Once the profile list resolves, upgrade the create form from 跟随全局 (0) to the
    // Debian preset — but only while the field is still pristine (0), so we never
    // clobber a selection the user already made in-progress. Runs once per mount
    // (ref-guarded), and the create dialog remounts on every open. Edit forms live in
    // CardContent.tsx and are unaffected by this.
    useEffect(() => {
        if (appliedDebianDefault.current) return;
        if (debianProfileId === 0) return;
        appliedDebianDefault.current = true;
        setFormData((prev) => (prev.cloak_profile_id === 0 ? { ...prev, cloak_profile_id: debianProfileId } : prev));
    }, [debianProfileId]);

    const handleSubmit = (event: React.FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const normalizedBaseUrls = (formData.base_urls ?? []).filter((u) => u.url.trim()).map((u) => ({
            url: normalizeBaseUrlForChannelType(formData.type, u.url),
            delay: Number(u.delay || 0),
        }));
        const normalizedKeys = formData.keys
            .filter((k) => k.channel_key.trim())
            .map((k) => ({ enabled: k.enabled, channel_key: k.channel_key, remark: k.remark ?? '' }));
        const normalizedHeaders = (formData.custom_header ?? [])
            .map((h) => ({ header_key: h.header_key.trim(), header_value: h.header_value }))
            .filter((h) => h.header_key && h.header_value !== '');

        const channelProxy = formData.channel_proxy.trim();
        const paramOverride = formData.param_override.trim();
        const systemPromptOverride = formData.system_prompt_override.trim();
        createChannel.mutate(
            {
                name: formData.name,
                type: formData.type,
                priority: formData.priority,
                max_concurrent: formData.max_concurrent,
                rpm_limit: formData.rpm_limit,
                key_select_strategy: formData.key_select_strategy,
                disable_circuit_breaker: formData.disable_circuit_breaker,
                race_mode: formData.race_mode,
                race_key_concurrency: formData.race_key_concurrency,
                race_delay_ms: formData.race_delay_ms,
                enabled: formData.enabled,
                base_urls: normalizedBaseUrls,
                keys: normalizedKeys,
                model: formData.model,
                custom_model: formData.custom_model,
                discovered_models: formData.discovered_models,
                selected_models: formData.selected_models,
                anthropic_context_1m: formData.anthropic_context_1m,
                thinking_to_content: formData.thinking_to_content,
                proxy: formData.proxy,
                auto_sync: formData.auto_sync,
                custom_header: normalizedHeaders,
                cloak: { mode: formData.cloak_mode || 'auto', profile_id: formData.cloak_profile_id ?? 0 },
                channel_proxy: channelProxy,
                openai_chat_path: formData.openai_chat_path.trim(),
                openai_models_path: formData.openai_models_path.trim(),
                param_override: paramOverride,
                system_prompt_override: systemPromptOverride,
                prompt_override_mode: formData.prompt_override_mode,
                match_regex: formData.match_regex.trim(),
                // Persist the model-mapping table on create — without this the mapping the
                // user typed in the form was silently dropped and never saved.
                model_mapping: formData.model_mapping,
            },
            {
                onSuccess: () => {
                    setFormData({
                        name: '',
                        type: ChannelType.OpenAIChat,
                        priority: 0,
                        max_concurrent: 0,
                        rpm_limit: 0,
                        key_select_strategy: 0,
                        disable_circuit_breaker: false,
                        race_mode: false,
                        race_key_concurrency: 2,
                        race_delay_ms: 0,
                        base_urls: [{ url: '', delay: 0 }],
                        custom_header: [],
                        cloak_mode: 'auto',
                        cloak_profile_id: 0,
                        channel_proxy: '',
                        param_override: '',
                        system_prompt_override: '',
                        prompt_override_mode: 'append_system',
                        keys: [{ enabled: true, channel_key: '', remark: '' }],
                        model: '',
                        custom_model: '',
                        discovered_models: [],
                        selected_models: [],
                        anthropic_context_1m: false,
                        thinking_to_content: false,
                        auto_sync: false,
                        enabled: true,
                        proxy: false,
                        match_regex: '',
                        openai_chat_path: '',
                        openai_models_path: '',
                    });
                    setIsOpen(false);
                }
            });
    };

    return (
        <div className="flex h-full w-[calc(100vw-1rem)] max-w-full min-h-0 flex-col md:max-w-xl">
            <MorphingDialogTitle className="shrink-0">
                <header className="mb-6 flex items-center justify-between">
                    <h2 className="text-2xl font-bold text-card-foreground">{t('dialogTitle')}</h2>
                    <MorphingDialogClose
                        className="relative right-0 top-0"
                        variants={{
                            initial: { opacity: 0, scale: 0.8 },
                            animate: { opacity: 1, scale: 1 },
                            exit: { opacity: 0, scale: 0.8 }
                        }}
                    />
                </header>
            </MorphingDialogTitle>
            <MorphingDialogDescription disableLayoutAnimation className="flex-1 min-h-0 overflow-auto">
                <ChannelImportCSV />
                <ChannelForm
                    formData={formData}
                    onFormDataChange={setFormData}
                    onSubmit={handleSubmit}
                    isPending={createChannel.isPending}
                    submitText={t('submit')}
                    pendingText={t('submitting')}
                    idPrefix="new-channel"
                />
            </MorphingDialogDescription>
        </div>
    );
}
