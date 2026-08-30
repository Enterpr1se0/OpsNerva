import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useTranslation } from 'react-i18next'
import { isValidElement, type ReactNode } from 'react'

import { CopyablePre } from './CopyButton'
import { HighlightedCode, languageFromCodeClass } from './HighlightedCode'

type MarkdownCodeElement={children?:ReactNode;className?:string}

export function MarkdownMessage({content,scope='chat'}:{content:string;scope?:'chat'|'skills'}){
	const {t}=useTranslation()
	return <Markdown skipHtml remarkPlugins={[remarkGfm]} components={{
		a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
		img:({alt})=><span className="markdown-image-blocked">{t(`${scope}.blockedImage`,{alt:alt||t('common.image')})}</span>,
		pre:({children})=>{
			const codeElement=isValidElement<MarkdownCodeElement>(children)?children:undefined
			const code=String(codeElement?.props.children??'').replace(/\n$/,'')
			return <CopyablePre value={code}><HighlightedCode code={code} language={languageFromCodeClass(codeElement?.props.className)} autoDetect={!codeElement?.props.className}/></CopyablePre>
		},
	}}>{content}</Markdown>
}
